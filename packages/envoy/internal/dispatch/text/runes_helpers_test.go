package text

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

// headRunesReference is HeadRunes as its comment defines it, with none of its shortcuts: a value
// of at most limit runes is returned unchanged, and a longer one is written out rune by rune up
// to the limit. The decoder reads a byte that is not UTF-8 as a rune of its own, U+FFFD, so it
// is written as U+FFFD, and every other rune is written as its own bytes.
func headRunesReference(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	var head strings.Builder
	for ; limit > 0; limit-- {
		r, size := utf8.DecodeRuneInString(value)
		head.WriteRune(r)
		value = value[size:]
	}
	return head.String()
}

// shapeOf names a value as headRunesReference reads it: whether the limit cuts it, and where it
// holds a real U+FFFD or a byte that is not UTF-8: in the part kept, as the first rune left out,
// or further on.
func shapeOf(value string, limit int) string {
	if value == "" {
		return "an empty value"
	}
	shape := "a value at or under the limit"
	if utf8.RuneCountInString(value) > limit {
		shape = "a cut"
	}
	places := [...]string{"kept", "right after the cut", "further on"}
	kinds := [...]string{"a real U+FFFD", "a byte that is not UTF-8"}
	var held [len(places)][len(kinds)]bool
	for n := 0; value != ""; n++ {
		r, size := utf8.DecodeRuneInString(value)
		value = value[size:]
		if r != utf8.RuneError {
			continue
		}
		place, kind := 0, 0
		if n == limit {
			place = 1
		} else if n > limit {
			place = 2
		}
		if size == 1 {
			kind = 1
		}
		held[place][kind] = true
	}
	var holds []string
	for place := range places {
		for kind := range kinds {
			if held[place][kind] {
				holds = append(holds, kinds[kind]+" "+places[place])
			}
		}
	}
	if len(holds) == 0 {
		return shape + " with no U+FFFD and no byte that is not UTF-8"
	}
	return shape + " with " + strings.Join(holds, ", ")
}

// headRunesAlphabet holds each byte that decides a cut: a letter; EF BF BD, the three bytes of
// U+FFFD, which alone or as a pair are bytes that are not UTF-8; FF, which is never UTF-8;
// E7 95 8C, the three bytes of 界; F0 9F, which with 95 8C are the four bytes of U+1F54C, a
// character outside the BMP; and C3 A9, the two bytes of é, each of which alone is not UTF-8.
const headRunesAlphabet = "a\xef\xbf\xbd\xff\xe7\x95\x8c\xf0\x9f\xc3\xa9"

// headRunesCharacters are the five characters that the alphabet's comment names: a, é, 界,
// U+FFFD and U+1F54C. The twelve bytes make 259 characters of one to four bytes; these five are
// the ones a seeded value is built of, with single bytes of the alphabet, so it holds runs of
// whole characters as well as bytes that are not UTF-8.
var headRunesCharacters = []string{"a", "é", "界", "\uFFFD", "\U0001F54C"}

// sink keeps each answer of a copy row, so an answer that the cut copied lives on the heap and is
// counted.
var sink string

// pair is a value, the limit it is cut to, and want, the number of bytes of the value that the cut
// keeps by the reference, for a pair whose answer is, by the reference, the value's own first bytes.
type pair struct {
	value       string
	limit, want int
}

// cutPairs gives value's two pairs whose answer is, by the reference, the value's own first bytes:
// the value cut to its own length in characters, which answers it whole, and cut to one character
// short, which answers all but its last character (for the empty value, the same pair twice). The
// length rows and the rows that cut every place make their pairs with it; it returns an array, so
// it allocates nothing.
func cutPairs(value string) [2]pair {
	characters := utf8.RuneCountInString(value)
	_, last := utf8.DecodeLastRuneInString(value)
	return [2]pair{{value, characters, len(value)}, {value, max(characters-1, 0), len(value) - last}}
}

// cutAt gives the pair of value, which is UTF-8, cut to limit characters, at most its own count,
// whose answer is, by the reference, the value's first limit characters: want counts their bytes,
// decoding them one at a time.
func cutAt(value string, limit int) pair {
	rest := value
	for range limit {
		_, size := utf8.DecodeRuneInString(rest)
		rest = rest[size:]
	}
	return pair{value, limit, len(value) - len(rest)}
}

// isSlice reports whether got is the value's own first want bytes: want bytes long and, unless it
// is empty, at the value's address. A copy has another address, whatever call made it.
func isSlice(got, value string, want int) bool {
	return len(got) == want && (want == 0 || unsafe.StringData(got) == unsafe.StringData(value))
}

// answerScan cuts each of pairs once in each of its runs, keeps each answer in sink, and counts the
// answers that are not the value's own first want bytes, which is, by the reference, each pair's
// answer, keeping the first of them. The two copy helpers below run it on their own schedules and
// report through it, so a bad answer is read and named in one place.
type answerScan struct {
	pairs                                      []pair
	runs, copies, first, firstRun, firstLength int
	firstAt                                    string
}

func (s *answerScan) run() {
	s.runs++
	for i, p := range s.pairs {
		got := HeadRunes(p.value, p.limit)
		if !isSlice(got, p.value, p.want) {
			if s.copies == 0 {
				s.first, s.firstRun, s.firstLength, s.firstAt = i, s.runs, len(got), "another address"
				if len(got) > 0 && unsafe.StringData(got) == unsafe.StringData(p.value) {
					s.firstAt = "the value's address"
				}
			}
			s.copies++
		}
		sink = got
	}
}

// check fails t when the row gave it no pair, since a row with no pair would check nothing and
// pass, whoever wrote it; and when any answer of any run was not the value's own first bytes,
// naming the first.
func (s *answerScan) check(t *testing.T) {
	t.Helper()
	if len(s.pairs) == 0 {
		t.Fatal("the row gave no pair to cut, so it would check nothing")
	}
	if s.copies > 0 {
		p := s.pairs[s.first]
		t.Fatalf("%d of the %d answers are not the value's own first bytes; the first, in run %d of %d: HeadRunes(%.70q, %d) answered %d bytes at %s, want the value's own first %d bytes (the value is %d bytes)", s.copies, s.runs*len(s.pairs), s.firstRun, s.runs, p.value, p.limit, s.firstLength, s.firstAt, p.want, len(p.value))
	}
}

// copiesNothing fails t when HeadRunes answers any of pairs with anything but the value's own first
// want bytes, which is, by the reference, each pair's answer. testing.AllocsPerRun cuts all the
// pairs 22 times, the first run uncounted, and each answer of each run is checked: so a copy fails
// on whichever of a pair's first 22 calls makes it, on each call, on the first only, the second
// only, each eleventh, or kept and returned on a later call. A row that cuts a pair before it calls
// copiesNothing checks that answer too. The count of the 21 counted runs adds what an address
// cannot show, an allocation that HeadRunes makes and does not return (a []rune of the value, say):
// it fails the row when the pairs make 21 or more of them over the 21 runs, since the mean, a whole
// number, leaves out fewer than 21 allocations of something else. Fewer such allocations, and a
// copy made only on a pair's 23rd call or later, can pass. It then counts the pairs one at a time
// to name the pair that allocated.
func copiesNothing(t *testing.T, pairs []pair) {
	t.Helper()
	scan := answerScan{pairs: pairs}
	allocations := testing.AllocsPerRun(21, scan.run)
	scan.check(t)
	if allocations == 0 {
		return
	}
	for _, p := range pairs {
		if allocations := testing.AllocsPerRun(21, func() { sink = HeadRunes(p.value, p.limit) }); allocations != 0 {
			t.Fatalf("HeadRunes(%.70q, %d) made %v allocations on each call, though each answer is the value's own first bytes: it allocates what it does not return (the value is %d bytes)", p.value, p.limit, allocations, len(p.value))
		}
	}
	t.Fatalf("the %d pairs made %v allocations in each run together and none alone, though each answer is the value's own first bytes", len(pairs), allocations)
}

// answersSlices fails t when HeadRunes answers any of pairs, on one call of each, with anything but
// the value's own first want bytes, which is, by the reference, each pair's answer. That one call
// catches a copy keyed on what a value of its pairs holds, on each call or on the first. A copy
// keyed on how many times a pair was cut is copiesNothing's to catch, over fewer pairs, and so is
// an allocation that HeadRunes makes and does not return.
func answersSlices(t *testing.T, pairs []pair) {
	t.Helper()
	scan := answerScan{pairs: pairs}
	scan.run()
	scan.check(t)
}

// keptCuts cuts values that are UTF-8, each kept+rest, to the code points of kept, and counts the
// cuts that keep something else, or keep it at another address: by the definition, that cut of a
// value that is UTF-8 is kept, the value's own first bytes, which HeadRunes answers as a slice of
// the value. So the rows that cut with it read every code point they cut for a copy too.
type keptCuts struct {
	t            *testing.T
	cuts, differ int
}

func (c *keptCuts) cut(kept, rest string) {
	c.cuts++
	value, limit := kept+rest, utf8.RuneCountInString(kept)
	if got := HeadRunes(value, limit); got != kept || !isSlice(got, value, len(kept)) {
		if c.differ < 10 && got == kept {
			c.t.Errorf("HeadRunes(%+q, %d) = %+q at another address, want the value's own first %d bytes", value, limit, got, len(kept))
		} else if c.differ < 10 {
			c.t.Errorf("HeadRunes(%+q, %d) = %+q, want %+q", value, limit, got, kept)
		}
		c.differ++
	}
}

// check fails the row when a cut kept something else or a copy, or when the row made no cut at all.
func (c *keptCuts) check(where string) {
	if c.cuts == 0 {
		c.t.Fatalf("no cut %s was made", where)
	}
	if c.differ > 0 {
		c.t.Errorf("%d of %d cuts %s differ from the characters before it, or are a copy of them", c.differ, c.cuts, where)
	}
}

// wantCount stops t when name, a list of inputs or the cuts or pairs of a row or a loop, comes to
// got entries, not the want written with it. A check of each entry cannot see a list emptied or
// short, nor a loop that runs over fewer entries, and the row would then cut fewer and still pass;
// unseen says what it would leave out.
func wantCount(t *testing.T, name string, got, want int, entries, unseen string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: %d %s, not the %d written with it, so %s", name, got, entries, want, unseen)
	}
}

// unicodeTables is the version of Unicode of the tables of Go's unicode package (unicode.Version)
// that the counts of the rows that read letters and marks from them were written from, with the
// module's toolchain.
const unicodeTables = "17.0.0"

// wantTables stops t when Go's unicode tables are of another version than unicodeTables: the
// row's counts would then differ for that reason alone, so the row names the version instead.
func wantTables(t *testing.T) {
	t.Helper()
	if unicode.Version != unicodeTables {
		t.Fatalf("Go's unicode tables are of Unicode %s, not the %s this row's counts were written from: count them again", unicode.Version, unicodeTables)
	}
}

// spanRunes lists the code points of each range of bounds, given as its first and its last.
func spanRunes(bounds ...rune) []rune {
	var runes []rune
	for i := 0; i < len(bounds); i += 2 {
		for r := bounds[i]; r <= bounds[i+1]; r++ {
			runes = append(runes, r)
		}
	}
	return runes
}

// tableRunes lists the code points of a table of Go's unicode package, in order.
func tableRunes(table *unicode.RangeTable) []rune {
	var runes []rune
	for _, r := range table.R16 {
		for c := rune(r.Lo); c <= rune(r.Hi); c += rune(r.Stride) {
			runes = append(runes, c)
		}
	}
	for _, r := range table.R32 {
		for c := rune(r.Lo); c <= rune(r.Hi); c += rune(r.Stride) {
			runes = append(runes, c)
		}
	}
	return runes
}
