package specform

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cputest"
)

// unclosedRuns is words that hold m runs of backticks that nothing closes, of the lengths 2 to
// m+1, and after them m*m spans of code of one backtick each: about m*m/2 backticks of runs that
// nothing closes and 4*m*m bytes of code. A reading that looks for the end of each run that
// nothing closes in the words after it reads those m*m spans once for each of the m runs.
func unclosedRuns(m int) string {
	var words strings.Builder
	for length := m + 1; length >= 2; length-- {
		words.WriteString("x" + strings.Repeat("`", length))
	}
	words.WriteString(" ")
	for range m * m {
		words.WriteString("`a` ")
	}
	return words.String()
}

// withoutCodeFontByScan is withoutCodeFont as it was first written, which looks for the run that
// closes each run in the words after it: the answer that withoutCodeFont must give, for words of
// any form, and work for each pair of runs where many runs are closed by none.
func withoutCodeFontByScan(words string) string {
	var out strings.Builder
	for at := 0; at < len(words); {
		if words[at] != '`' {
			next := strings.IndexByte(words[at:], '`')
			if next < 0 {
				next = len(words) - at
			}
			out.WriteString(words[at : at+next])
			at += next
			continue
		}
		fence := backticksAt(words, at)
		closing := -1
		for search := at + fence; search < len(words); {
			next := strings.IndexByte(words[search:], '`')
			if next < 0 {
				break
			}
			search += next
			if run := backticksAt(words, search); run == fence {
				closing = search
				break
			} else {
				search += run
			}
		}
		if closing < 0 {
			out.WriteString(words[at : at+fence])
			at += fence
			continue
		}
		code := words[at+fence : closing]
		if len(code) > 1 && code[0] == ' ' && code[len(code)-1] == ' ' && strings.Trim(code, " ") != "" {
			code = code[1 : len(code)-1]
		}
		out.WriteString(code)
		at = closing + fence
	}
	return out.String()
}

// withoutCodeFont gives the words that withoutCodeFontByScan gives, for words of backticks, spaces
// and letters in any order and runs of any length, a run that nothing closes among them.
func TestTheWordsWithNoCodeFontAreTheWordsOfTheFirstReading(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	pieces := []string{"`", "``", "```", " ", "  ", "a", "b c", ".", "`` ", " `"}
	for range 20000 {
		var words strings.Builder
		for range random.IntN(24) {
			words.WriteString(pieces[random.IntN(len(pieces))])
		}
		if got, want := withoutCodeFont(words.String()), withoutCodeFontByScan(words.String()); got != want {
			t.Fatalf("withoutCodeFont(%q) = %q, want %q", words.String(), got, want)
		}
	}
	for _, m := range []int{0, 1, 2, 5} {
		if got, want := withoutCodeFont(unclosedRuns(m)), withoutCodeFontByScan(unclosedRuns(m)); got != want {
			t.Fatalf("withoutCodeFont(unclosedRuns(%d)) = %q, want %q", m, got, want)
		}
	}
}

// The work of withoutCodeFont grows in proportion to its words, also for words that hold many runs
// of backticks that nothing closes: words of workScale times the bytes (unclosedRuns of four times
// m) cost less than workBound times the work, as each row of the work table does. A reading that
// looks for the end of each such run in the words after it does work for each pair of a run and a
// span after it, which grows about 64 times. Under the race detector it reads the words and claims
// no bound.
func TestTheWorkOfTheWordsWithNoCodeFontGrowsInProportionToTheWords(t *testing.T) {
	m := 128
	if raceEnabled {
		m = 32
	}
	small, large := unclosedRuns(m), unclosedRuns(4*m)
	if ratio := float64(len(large)) / float64(len(small)); ratio < 15 || ratio > 17 {
		t.Fatalf("the larger words are %.1f times the smaller, want about %d", ratio, workScale)
	}
	var kept [2]string
	growth, smallWork, largeWork := cputest.Growth(workSamples, workScale,
		cputest.Again(func() { kept[0] = withoutCodeFont(small) }), cputest.Again(func() { kept[1] = withoutCodeFont(large) }))
	t.Logf("%d bytes %d times: %v; %d bytes once: %v; %.1f times", len(small), workScale, smallWork, len(large), largeWork, growth)
	if strings.Count(kept[1], "`") != (4*m+2)*(4*m+1)/2-1 {
		t.Fatalf("withoutCodeFont of the larger words keeps %d backticks, want each of the runs that nothing closes and none of the code", strings.Count(kept[1], "`"))
	}
	if !raceEnabled && growth >= workBound {
		t.Errorf("the work grew %.1f times, from %v for %d runs of %d bytes to %v for one run of %d, want less than %d times",
			growth, smallWork, workScale, len(small), largeWork, len(large), workBound)
	}
}
