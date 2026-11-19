package text

import (
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
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

// sink keeps each answer of an allocation row, so an answer that the cut copied lives on the heap
// and is counted.
var sink string

// pair is a value and the limit it is cut to.
type pair struct {
	value string
	limit int
}

// copiesNothing fails t when HeadRunes makes an allocation for any of pairs, each of which has an
// answer that is, by the reference, a slice of its value. It counts all of them at once, then one
// at a time to name the pair that copied.
func copiesNothing(t *testing.T, pairs []pair) {
	t.Helper()
	if testing.AllocsPerRun(10, func() {
		for _, p := range pairs {
			sink = HeadRunes(p.value, p.limit)
		}
	}) == 0 {
		return
	}
	for _, p := range pairs {
		if allocations := testing.AllocsPerRun(10, func() { sink = HeadRunes(p.value, p.limit) }); allocations != 0 {
			t.Fatalf("HeadRunes(%.70q, %d) made %v allocations, want a slice of the value and none (the value is %d bytes)", p.value, p.limit, allocations, len(p.value))
		}
	}
	t.Fatalf("the %d pairs made allocations together and none alone", len(pairs))
}

// keptCuts cuts values that are UTF-8, each kept+rest, to the code points of kept, and counts the
// cuts that keep something else: by the definition, that cut of a value that is UTF-8 is kept.
type keptCuts struct {
	t            *testing.T
	cuts, differ int
}

func (c *keptCuts) cut(kept, rest string) {
	c.cuts++
	value, limit := kept+rest, utf8.RuneCountInString(kept)
	if got := HeadRunes(value, limit); got != kept {
		if c.differ < 10 {
			c.t.Errorf("HeadRunes(%+q, %d) = %+q, want %+q", value, limit, got, kept)
		}
		c.differ++
	}
}

// check fails the row when a cut kept something else, or when the row made no cut at all.
func (c *keptCuts) check(where string) {
	if c.cuts == 0 {
		c.t.Fatalf("no cut %s was made", where)
	}
	if c.differ > 0 {
		c.t.Errorf("%d of %d cuts %s differ from the characters before it", c.differ, c.cuts, where)
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

// HeadRunes counts a rune of any script as one, and a document edit's read-back cuts its last
// block with it. The first row compares it with headRunesReference on every value of 0 to 5 bytes
// of the alphabet, to each limit from 0 to 6: every pair, no sample. Five bytes is the shortest
// value on which a guard that reads the start of the value, not the place of the rune, changes
// what HeadRunes does: it copies the cut of a letter, U+FFFD and a letter, and it keeps as it is a
// byte that is not UTF-8 after a U+FFFD that starts the value.
//
// The second row compares the two on seeded values of 6 to 64 bytes, to each limit from 0 to 70.
// The seed is fixed, so a failure repeats. The third puts each of six kinds of character at each
// place of a value of 2,001 characters (a byte that is not UTF-8, U+FFFD, é, 界, U+1F54C and a
// combining mark), and cuts the value at each limit from one character before that place to two
// after it, and at 2,000. A guard that reads only the first N runes or the first N bytes of the
// value fails the third row for each N under 2,000; the second row fails the first guard only for
// N up to 24, and the second for N up to 61. Both rows also want no allocation for each of their
// pairs whose answer is, by the reference, the value's own first bytes, a value returned
// unchanged among them, as the shape rows below want for the pairs of the first row.
//
// The fourth row cuts before each code point but the surrogates, after the same code point and
// after each of 62 strings of one or two code points, and wants the cut to leave the code point
// out. The strings are a letter, an emoji that takes a skin-tone modifier, and the left side of
// each rule of Unicode Standard Annex #29 that keeps two code points in one grapheme cluster: CR
// (GB3); the first Hangul L, V, T, LV and LVT, and the first Kirat Rai V (GB6 to GB8); each of the
// 27 Prepends (GB9b) and each of the 23 linkers, after the first consonant of its script (GB9c),
// since GB9b keeps any code point but a control after a Prepend, and GB9c any consonant after a
// linker; two pictographs, each with a zero-width joiner (GB11); and the first regional indicator
// (GB12 and GB13). The fifth row cuts between the two code points of each pair that GB6 to GB8,
// GB12 and GB13 keep together, every one of them; before each letter of the script of a linker's
// consonant, after each letter of that script and the linker; and before the first pictograph,
// after each code point and a joiner. The sixth cuts before each code point that GB9 and GB9a
// keep after any other, a combining mark or one of 107 more, after each letter of its own script,
// and after each letter among its 128 code points (those that share all but its last seven bits),
// so that a mark of the script Inherited, which takes the script of the letter before it, is cut
// after the letters near it. So a count that keeps a code point with one of these before it fails
// a row, whatever its length: a character is one code point, not a grapheme cluster. A count keyed
// on code points that no row cuts together can pass, such as a mark after a code point that is no
// letter of its script and not among its 128 code points (U+0301 after e), a consonant after a
// linker and a letter of another script, or a pictograph after another and a joiner, neither of
// them U+00A9 or U+1F44D.
//
// The code points that a rule names are written here from the data of Unicode 18.0, since Go's
// unicode package gives no grapheme property. The letters, the marks and their scripts are read
// from Go's own tables (unicode.Version, 17.0.0 with the module's toolchain), since they are too
// many to write here: a letter or a mark that is new in 18.0 is neither there, and only the fourth
// row cuts before it.
//
// An answer that is, by the reference, the value's own first bytes is a slice of the value, and
// HeadRunes copies nothing for it. One row for each shape of such a pair (shapeOf) cuts every
// pair of that shape and wants no allocation, so a copy of any one pair fails its row; two rows
// cut a value of letters and one that starts with 界, at each length from 0 to 8,001 bytes and at
// every 32nd from 8,001 to 32,001, to its own length and to one character short. The last rows
// hold values longer than five bytes, each cut to a slice with no copy, up to 32,000 characters,
// the most an embedding input keeps. Among them, a cut after characters outside the BMP fails a
// count of UTF-16 units, and a cut before a combining mark fails a count that keeps a mark with
// its letter.
func TestHeadRunesCutsAtARuneAndCopiesNothing(t *testing.T) {
	const longest, limits = 5, 6
	var values []string
	for length := 0; length <= longest; length++ {
		count := 1
		for range length {
			count *= len(headRunesAlphabet)
		}
		for code := range count {
			value := make([]byte, length)
			for i, digits := 0, code; i < length; i, digits = i+1, digits/len(headRunesAlphabet) {
				value[i] = headRunesAlphabet[digits%len(headRunesAlphabet)]
			}
			values = append(values, string(value))
		}
	}

	t.Run("every pair agrees with the reference", func(t *testing.T) {
		differ := 0
		for _, value := range values {
			for limit := 0; limit <= limits; limit++ {
				got, want := HeadRunes(value, limit), headRunesReference(value, limit)
				if got == want {
					continue
				}
				if differ < 10 {
					t.Errorf("HeadRunes(%q, %d) = %q, want %q", value, limit, got, want)
				}
				differ++
			}
		}
		if differ > 0 {
			t.Errorf("%d of %d pairs differ from the reference", differ, len(values)*(limits+1))
		}
	})

	t.Run("every seeded value of 6 to 64 bytes agrees with the reference, and copies nothing where its answer is a slice", func(t *testing.T) {
		const seeded, longLimits = 20000, 70
		for _, character := range headRunesCharacters {
			for i := range len(character) {
				if strings.IndexByte(headRunesAlphabet, character[i]) < 0 {
					t.Fatalf("%q holds the byte %#x, which the alphabet does not", character, character[i])
				}
			}
		}
		random := rand.New(rand.NewPCG(1, 6))
		differ := 0
		var sliced []pair
		for range seeded {
			length := 6 + random.IntN(59)
			built := make([]byte, 0, length+3)
			for len(built) < length {
				if random.IntN(4) == 0 {
					built = append(built, headRunesAlphabet[random.IntN(len(headRunesAlphabet))])
				} else {
					built = append(built, headRunesCharacters[random.IntN(len(headRunesCharacters))]...)
				}
			}
			value := string(built[:length])
			for limit := 0; limit <= longLimits; limit++ {
				got, want := HeadRunes(value, limit), headRunesReference(value, limit)
				if strings.HasPrefix(value, want) {
					sliced = append(sliced, pair{value, limit})
				}
				if got == want {
					continue
				}
				if differ < 10 {
					t.Errorf("HeadRunes(%q, %d) = %q, want %q", value, limit, got, want)
				}
				differ++
			}
		}
		if differ > 0 {
			t.Errorf("%d of %d seeded pairs differ from the reference", differ, seeded*(longLimits+1))
		}
		copiesNothing(t, sliced)
	})

	t.Run("each of six kinds of character at each place of 2,001 runes agrees with the reference, and copies nothing where its answer is a slice", func(t *testing.T) {
		const characters = 2001
		differ, cuts := 0, 0
		var sliced []pair
		for _, kind := range []string{"\xff", "\uFFFD", "é", "界", "\U0001F54C", "\u0301"} {
			for place := range characters {
				value := strings.Repeat("a", place) + kind + strings.Repeat("a", characters-1-place)
				around := []int{place - 1, place, place + 1, place + 2, characters - 1}
				for i, limit := range around {
					if limit < 0 || slices.Contains(around[:i], limit) {
						continue
					}
					cuts++
					got, want := HeadRunes(value, limit), headRunesReference(value, limit)
					if strings.HasPrefix(value, want) {
						sliced = append(sliced, pair{value, limit})
					}
					if got == want {
						continue
					}
					if differ < 10 {
						from := min(max(0, place-5), len(got), len(want))
						t.Errorf("%+q at rune %d, cut to %d: HeadRunes kept %d bytes, %.20q... from byte %d, want %d bytes, %.20q...", kind, place, limit, len(got), got[from:], from, len(want), want[from:])
					}
					differ++
				}
			}
		}
		if differ > 0 {
			t.Errorf("%d of %d cuts differ from the reference", differ, cuts)
		}
		copiesNothing(t, sliced)
	})

	// linkers are the 23 linkers of Unicode 18.0 (GB9c), each after the first consonant of its
	// script, Devanagari's for the two Vedic signs of Common.
	linkers := []string{
		// Devanagari, Bengali, Gujarati, Oriya, Telugu, Malayalam, Myanmar, Khmer, Tai Tham,
		// Balinese, Sundanese, Common (two Vedic signs), Javanese, Meetei Mayek, Kharoshthi, Chakma,
		// Tulu-Tigalari, Dives Akuru, Zanabazar Square (two), Soyombo and Kawi.
		"\u0915\u094D", "\u0995\u09CD", "\u0A95\u0ACD", "\u0B15\u0B4D", "\u0C15\u0C4D", "\u0D15\u0D4D", "\u1000\u1039",
		"\u1780\u17D2", "\u1A20\u1A60", "\u1B0B\u1B44", "\u1B83\u1BAB", "\u0915\u1CF5", "\u0915\u1CF6", "\uA989\uA9C0",
		"\uAAE0\uAAF6", "\U00010A00\U00010A3F", "\U00011103\U00011133", "\U00011380\U000113D0", "\U00011900\U0001193E",
		"\U00011A00\U00011A3A", "\U00011A00\U00011A47", "\U00011A50\U00011A99", "\U00011F04\U00011F42",
	}
	// letters holds the letters of each script of Go's tables, under the script's name.
	letters := map[string][]rune{}
	for name, table := range unicode.Scripts {
		for _, r := range tableRunes(table) {
			if unicode.IsLetter(r) {
				letters[name] = append(letters[name], r)
			}
		}
	}

	t.Run("a cut before each code point leaves it out, after the code point itself and after each of 62 strings of one or two code points", func(t *testing.T) {
		befores := append([]string{
			// Any character (GB9, GB9a), and an emoji that takes a skin-tone modifier.
			"a", "\U0001F44D",
			// GB3: CR. GB6 to GB8: the first L, V, LV, T and LVT of Hangul, and the first V of Kirat Rai.
			"\r", "\u1100", "\u1160", "\U00016D63", "\uAC00", "\u11A8", "\uAC01",
			// GB9b: each Prepend, of Arabic, Common, Syriac, Malayalam, Kaithi, Sharada, Tulu-Tigalari,
			// Dives Akuru, Soyombo, Masaram Gondi and Kawi.
			"\u0600", "\u0601", "\u0602", "\u0603", "\u0604", "\u0605", "\u06DD", "\u070F", "\u0890", "\u0891", "\u08E2",
			"\u0D4E", "\U000110BD", "\U000110CD", "\U000111C2", "\U000111C3", "\U000113D1", "\U0001193F", "\U00011941",
			"\U00011A84", "\U00011A85", "\U00011A86", "\U00011A87", "\U00011A88", "\U00011A89", "\U00011D46", "\U00011F02",
			// GB11: a pictograph and a zero-width joiner. GB12 and GB13: a regional indicator.
			"\u00A9\u200D", "\U0001F44D\u200D", "\U0001F1E6",
		}, linkers...)
		cuts := keptCuts{t: t}
		for r := rune(0); r <= utf8.MaxRune; r++ {
			if !utf8.ValidRune(r) {
				continue
			}
			character := string(r)
			rest := character + "a"
			cuts.cut(character+character, character)
			for _, before := range befores {
				cuts.cut(before, rest)
			}
		}
		cuts.check("before a code point")
	})

	t.Run("a cut before a code point that a grapheme rule joins to each code point of its left side leaves it out", func(t *testing.T) {
		// GB6 to GB8: the Hangul L, V and T of Unicode 18.0 (GraphemeBreakProperty.txt), with the
		// Kirat Rai V among the V, and the syllables U+AC00 to U+D7A3, of which LV are those whose
		// distance from U+AC00 is a multiple of 28, and LVT the rest. GB12 and GB13: the regional
		// indicators.
		leading := spanRunes(0x1100, 0x115F, 0xA960, 0xA97C)
		vowels := spanRunes(0x1160, 0x11A7, 0xD7B0, 0xD7C6, 0x16D63, 0x16D63, 0x16D67, 0x16D6A)
		trailing := spanRunes(0x11A8, 0x11FF, 0xD7CB, 0xD7FB)
		regional := spanRunes(0x1F1E6, 0x1F1FF)
		var lv, lvt []rune
		for syllable := rune(0xAC00); syllable <= 0xD7A3; syllable++ {
			if (syllable-0xAC00)%28 == 0 {
				lv = append(lv, syllable)
			} else {
				lvt = append(lvt, syllable)
			}
		}
		cuts := keptCuts{t: t}
		for _, rule := range []struct{ left, right []rune }{
			{leading, slices.Concat(leading, vowels, lv, lvt)},
			{slices.Concat(lv, vowels), slices.Concat(vowels, trailing)},
			{slices.Concat(lvt, trailing), trailing},
			{regional, regional},
		} {
			for _, left := range rule.left {
				for _, right := range rule.right {
					cuts.cut(string(left), string(right)+"a")
				}
			}
		}
		// GB9c: each letter of the script of a linker's consonant, the linker, then each letter of
		// that script, which holds each consonant of the script that Go's tables call a letter.
		for _, linked := range linkers {
			consonant, size := utf8.DecodeRuneInString(linked)
			for name, table := range unicode.Scripts {
				if unicode.Is(table, consonant) {
					for _, letter := range letters[name] {
						for _, next := range letters[name] {
							cuts.cut(string(letter)+linked[size:], string(next)+"a")
						}
					}
				}
			}
		}
		// GB11: each code point and a joiner, then the first pictograph.
		for r := rune(0); r <= utf8.MaxRune; r++ {
			if utf8.ValidRune(r) {
				cuts.cut(string(r)+"\u200D", "\u00A9a")
			}
		}
		cuts.check("before a code point that a grapheme rule joins to the one before it")
	})

	t.Run("a cut before each mark that a grapheme rule keeps after any code point, after each letter of its own script or of its 128 code points, leaves it out", func(t *testing.T) {
		// GB9 and GB9a keep after any code point each combining mark and, of Unicode 18.0
		// (GraphemeBreakProperty.txt), these 107 code points that are no mark in Go's tables: the
		// spacing marks of Thai and Lao that are letters, the two joiners, the halfwidth sound
		// marks, the emoji modifiers and the tags.
		others := map[rune]bool{}
		for _, r := range spanRunes(0x0E33, 0x0E33, 0x0EB3, 0x0EB3, 0x200C, 0x200D, 0xFF9E, 0xFF9F, 0x1F3FB, 0x1F3FF, 0xE0020, 0xE007F) {
			others[r] = true
		}
		joins := func(r rune) bool { return unicode.Is(unicode.M, r) || others[r] }
		cuts := keptCuts{t: t}
		for _, name := range slices.Sorted(maps.Keys(unicode.Scripts)) {
			for _, mark := range tableRunes(unicode.Scripts[name]) {
				if joins(mark) {
					for _, letter := range letters[name] {
						cuts.cut(string(letter), string(mark)+"a")
					}
				}
			}
		}
		for mark := rune(0); mark <= unicode.MaxRune; mark++ {
			if joins(mark) {
				for letter := mark &^ 0x7F; letter < mark&^0x7F+0x80; letter++ {
					if unicode.IsLetter(letter) {
						cuts.cut(string(letter), string(mark)+"a")
					}
				}
			}
		}
		cuts.check("before a mark")
	})

	// lengthPairs gives two values, one of letters and one that starts with 界, each at every
	// step-th length from from to to bytes, cut to its own length in characters and to one short.
	lengthPairs := func(from, to, step int) []pair {
		var pairs []pair
		for _, first := range []string{"", "界"} {
			long := first + strings.Repeat("a", to-len(first))
			for n := max(from, len(first)); n <= to; n += step {
				characters := utf8.RuneCountInString(long[:n])
				pairs = append(pairs, pair{long[:n], characters}, pair{long[:n], max(characters-1, 0)})
			}
		}
		return pairs
	}
	t.Run("a value of each length up to 8,001 bytes, unchanged or cut one short, copies nothing", func(t *testing.T) {
		copiesNothing(t, lengthPairs(0, 8001, 1))
	})
	t.Run("a value of every 32nd length from 8,001 to 32,001 bytes, unchanged or cut one short, copies nothing", func(t *testing.T) {
		copiesNothing(t, lengthPairs(8001, 32001, 32))
	})

	shapes := map[string][]pair{}
	for _, value := range values {
		for limit := 0; limit <= limits; limit++ {
			if strings.HasPrefix(value, headRunesReference(value, limit)) {
				shape := shapeOf(value, limit)
				shapes[shape] = append(shapes[shape], pair{value, limit})
			}
		}
	}
	for _, shape := range slices.Sorted(maps.Keys(shapes)) {
		pairs := shapes[shape]
		t.Run(shape+" copies nothing", func(t *testing.T) { copiesNothing(t, pairs) })
	}

	for _, row := range []struct {
		name, value string
		limit       int
		want        string
	}{
		{name: "characters outside the BMP are cut at the limit", value: strings.Repeat("𠀀", 10), limit: 4, want: strings.Repeat("𠀀", 4)},
		{name: "2,001 runes of 界 are cut at 1,967", value: strings.Repeat("界", 2001), limit: 1967, want: strings.Repeat("界", 1967)},
		{name: "100 pairs of a letter and U+FFFD are cut at 50", value: strings.Repeat("a\uFFFD", 100), limit: 50, want: strings.Repeat("a\uFFFD", 25)},
		{name: "a byte that is not UTF-8 past the cut is left out", value: strings.Repeat("界", 10) + "\xff", limit: 5, want: strings.Repeat("界", 5)},
		{name: "a combining mark after the cut is left out", value: strings.Repeat("a\u0301", 10), limit: 3, want: "a\u0301a"},
		{name: "2,001 runes of 界 are returned unchanged", value: strings.Repeat("界", 2001), limit: 2001, want: strings.Repeat("界", 2001)},
		{name: "32,000 letters are returned unchanged", value: strings.Repeat("a", 32000), limit: 32000, want: strings.Repeat("a", 32000)},
		{name: "32,000 runes of 界 are returned unchanged", value: strings.Repeat("界", 32000), limit: 32000, want: strings.Repeat("界", 32000)},
		{name: "32,001 letters are cut at 32,000", value: strings.Repeat("a", 32001), limit: 32000, want: strings.Repeat("a", 32000)},
		{name: "32,001 runes of 界 are cut at 32,000", value: strings.Repeat("界", 32001), limit: 32000, want: strings.Repeat("界", 32000)},
	} {
		t.Run(row.name, func(t *testing.T) {
			allocations := testing.AllocsPerRun(100, func() { sink = HeadRunes(row.value, row.limit) })
			if sink != row.want {
				t.Fatalf("HeadRunes to %d kept %d bytes, %.40q, want %d bytes, %.40q", row.limit, len(sink), sink, len(row.want), row.want)
			}
			if allocations != 0 {
				t.Fatalf("HeadRunes to %d made %v allocations, want a slice of the value and none", row.limit, allocations)
			}
		})
	}
}
