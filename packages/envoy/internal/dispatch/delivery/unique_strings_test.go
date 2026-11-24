package delivery

import (
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/citebody"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/distinct"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// appendUniqueByScan is how delivery kept each value once before distinct.Strings: a scan of the
// kept list for each value. The differential row holds distinct.Strings to its answers; the
// distinct package's own rows count its steps.
func appendUniqueByScan(list []string, values ...string) []string {
	for _, value := range values {
		if !slices.Contains(list, value) {
			list = append(list, value)
		}
	}
	return list
}

// TestUniqueStringsAnswersAsTheScanDid is the differential row: over 2,000 sequences drawn with a
// fixed seed (pools of 1 to 5,000 values, so from all repeats to all distinct; values that differ
// only in case, in a trailing space or in a two-byte letter; the empty string), and over the
// values delivery's finders read from generated text (issue keys, session trailers),
// distinct.Strings keeps what the scan kept, in the same order, and uniqueKeys, sessionTrailers and
// citedIssues answer a non-nil list when they find nothing, as before.
func TestUniqueStringsAnswersAsTheScanDid(t *testing.T) {
	draw := rand.New(rand.NewPCG(5041, 2026))
	sequences := 0
	for range 2000 {
		pool := 1 + draw.IntN(5000)
		values := make([]string, draw.IntN(600))
		for i := range values {
			n := draw.IntN(pool)
			values[i] = [...]string{"v" + strconv.Itoa(n), "V" + strconv.Itoa(n), "v" + strconv.Itoa(n) + " ", "é" + strconv.Itoa(n), ""}[draw.IntN(5)]
		}
		kept := distinct.New()
		kept.Add(values...)
		if want := appendUniqueByScan([]string{}, values...); !slices.Equal(kept.List(), want) {
			t.Fatalf("sequence %d of %d values keeps %d values, want the scan's %d in its order", sequences, len(values), len(kept.List()), len(want))
		}
		sequences++
	}
	keyTexts, trailerTexts := 0, 0
	for range 500 {
		var text, messages strings.Builder
		for range draw.IntN(300) {
			key := [...]string{"ATTR", "INFRA", "LEGION", "attr"}[draw.IntN(4)] + "-" + strconv.Itoa(draw.IntN(400))
			text.WriteString(key + [...]string{" ", ", ", "\n", "x"}[draw.IntN(4)])
			messages.WriteString("fix: a change\n\nOmp-Session: " + strconv.Itoa(draw.IntN(200)) + "\n\x00")
		}
		if got, want := uniqueKeys(text.String()), appendUniqueByScan([]string{}, issueKeyCandidate.FindAllString(text.String(), -1)...); !slices.Equal(got, want) {
			t.Fatalf("uniqueKeys keeps %v, want the scan's %v", got, want)
		}
		keyTexts++
		split := strings.Split(messages.String(), "\x00")
		want := []string{}
		for _, message := range split {
			for _, match := range ompSessionTrailer.FindAllStringSubmatch(message, -1) {
				want = appendUniqueByScan(want, match[1])
			}
		}
		if got := sessionTrailers(split); !slices.Equal(got, want) {
			t.Fatalf("sessionTrailers keeps %v, want the scan's %v", got, want)
		}
		trailerTexts++
	}
	if uniqueKeys("") == nil || sessionTrailers(nil) == nil || citedIssues(refs.Renames{}, "acme/widgets", citebody.New("", nil)) == nil {
		t.Errorf("a reading that finds nothing answers nil, want an empty list: a nil stored input means it was never read")
	}
	t.Logf("%d sequences, %d key texts, %d trailer texts compared with the scan", sequences, keyTexts, trailerTexts)
}
