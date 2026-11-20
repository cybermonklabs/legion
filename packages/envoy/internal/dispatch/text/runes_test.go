package text

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

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
// N up to 24, and the second for N up to 61. Both rows also want each of their pairs whose answer
// is, by the reference, the value's own first bytes, a value returned unchanged among them, to
// answer that slice of the value itself, as the first row and the shape rows below want for the
// pairs of the first row.
//
// The fourth row cuts before each code point but the surrogates, after the same code point and
// after each of 62 strings of one or two code points, and wants the cut to leave the code point
// out. The strings are a letter, an emoji that takes a skin-tone modifier, and the left side of
// each rule of Unicode Standard Annex #29 that keeps two code points in one grapheme cluster: CR
// (GB3); the first Hangul L, V, T, LV and LVT, and the first Kirat Rai V (GB6 to GB8); each of the
// 27 Prepends (GB9b), since GB9b keeps any code point but a control after a Prepend; each of the
// 23 linkers, after the first consonant of its script or, for the two Vedic signs, whose script
// Common has no consonant, after Devanagari's (GB9c); two pictographs, each with a zero-width
// joiner (GB11); and the first regional indicator (GB12 and GB13). The fifth row cuts between the
// two code points of each pair that GB6 to GB8, GB12 and GB13 keep together, every one of them;
// before each letter of the script of a linker's consonant, after each letter of that script and
// the linker, and after the consonant, the linker and each code point the sixth row cuts before,
// since GB9c, as Unicode 18.0 writes it, keeps a consonant after a linker and any InCB=Extend, and
// those code points hold each InCB=Extend of Go's tables; and before the first pictograph, after
// each code point and a joiner. A linker that makes no cut fails the fifth row, so tables with no
// script for its consonant cannot make the row hold less unseen. The sixth cuts before each
// combining mark and each of 107 more code points, which GB9 and GB9a keep after any other but
// for 31 spacing marks of Myanmar, Tai Tham and Ahom, after each letter of its own script, and
// after each letter among its 128 code points (those that share all but its last seven bits); it
// cuts each of these of the script Inherited, which takes the script of the letter before it,
// after the first letter of each script, each mark of SignWriting, which has no letter, after each
// SignWriting base, and each emoji modifier after each emoji modifier base. So a count that keeps
// a code point with one of these before it fails a row, whatever its length: a character is one
// code point, not a grapheme cluster. A count keyed on code points that no row cuts together can
// pass, such as a mark after a code point that is no letter of its script and not among its 128
// code points (U+0301 after e, or a tag after U+1F3F4), an Inherited mark after a letter that is
// not the first of its script, a consonant that GB9c keeps after a linker in any case but the three
// the rows cut (each letter of the script of the linker's consonant after each letter of that
// script and the linker, or after the consonant, the linker and one code point of joined, and any
// code point after the consonant and the linker), so one of another script, one after a code point
// before the linker that is no letter of that script (U+093C, the nukta), one after a letter other
// than the consonant, the linker and an InCB=Extend, and one after two InCB=Extend, or a pictograph
// after another and a joiner, where the first is neither U+00A9 nor U+1F44D and the second is not
// U+00A9.
//
// The code points that a rule names are written here from the data of Unicode 18.0, since Go's
// unicode package gives no grapheme property. The letters, the marks and their scripts are read
// from Go's own tables (unicode.Version, 17.0.0 with the module's toolchain), since they are too
// many to write here: a letter or a mark that is new in 18.0 is neither there, and only the fourth
// row cuts before it.
//
// An answer that is, by the reference, the value's own first bytes is a slice of the value, and
// HeadRunes copies nothing for it. A copy has another address, so each row that holds such pairs
// wants each answer at the value's address and of the reference's length, the fourth, fifth and
// sixth rows too (keptCuts). So a copy keyed on a character fails a row whose values hold it: the
// fourth row puts each code point but the surrogates in values of 3 to 13 bytes, and the thirteenth
// puts prose, which holds each printable ASCII byte, a tab, a carriage return, a newline and 42
// characters of six scripts, outside the BMP too, in values of each length the seventh and eighth
// rows hold. The thirteenth and fourteenth rows cut their values, of prose and of letters, in the
// middle and at each limit a caller of HeadRunes passes as a constant (callerLimits), which the
// length rows below do not: a read-back cuts its last block to the room it has left, any count up
// to 2,000, which the middle cuts reach in a value of twice it, and an excerpt cuts a block to 240
// or 480 characters. The rows that call copiesNothing cut each pair 22 times, so a copy fails on
// whichever of a pair's first 22 calls makes it, and count the allocations of those calls; one of
// them for each shape of such a pair (shapeOf) cuts every pair of that shape. Two rows cut values
// at each length from 0 to 8,001 bytes, at every 32nd from 8,033 to 32,001 and at every 199th from
// 32,001 to 128,001, to their own length and to one character short, since an embedding input of
// any script reaches each of those lengths: one of letters and, for each of é, 界 and U+1F54C,
// characters of two, three and four bytes, values of letters and one run of the character or the
// character once (lengthPairs lists them): a run first or last with up to 66 letters or half the
// length on its other side, a run between one letter and up to four or between two quarters of the
// length, two of the character between two halves, the run alone, the character once first, last,
// in the middle, or with up to 63 letters on one side and the rest on the other, and the character
// once and a run first or last at one more place for each length, which the length's place in its
// row spreads evenly over the value in every row (lengthPairs says how). They cut each pair once
// (answersSlices), which catches a copy keyed on what a value of theirs holds; a third cuts the
// values of each kind they hold 22 times, at each edge of the ranges of lengths the rows hold and
// one byte each side of it, and at 500 and 1,000 bytes and one byte each side, which catches a copy
// keyed on how many times a pair was cut. Three more put the character once at every place, and a
// run beside every count of letters on one side of it: one cuts those pairs 22 times at each length
// the third cuts up to 2,005 bytes, the 2,000 characters a document edit reads back among them, one
// once at 5,000 bytes, at 8,001, where the seventh row ends, and at 8,033, where the eighth starts,
// and one once at 127,919, where the eighth ends. No argument from HeadRunes' code stands for the
// lengths, places or limits between (everyPlacePairs says why), so these can pass: a copy keyed on
// a value with two runs of wider characters or more, such as one at each end, or with characters
// of two widths besides letters, or on a run between two stretches of letters (say, ten letters, a
// run of three, then 5,000 letters), or on a count of letters beside one run or the character once
// at a length that no row cuts at every place, where the length rows put it at a few places only
// (say, the character once after exactly 100 letters, in values of 4,999 bytes only, or of 32,001);
// a copy keyed both on a character and on a length of more than 17 bytes, where the rows put only
// é, 界, U+1F54C, U+FFFD, U+20000, the characters of prose and, up to 64 bytes, those the alphabet's
// bytes make (say, a copy of each value of more than 17 bytes that holds ź, 漢 or U+1F680), or on a
// place of a character of prose that no value of the thirteenth row puts it at, since the row starts
// each of its values at one place of prose; a copy of a cut below the value's own count less one,
// keyed on a length and a limit that no row cuts together, since the thirteenth and fourteenth rows
// cut each length at its middle and at the callers' limits only, the third row its values of 2,001
// characters around one place, and the kept rows five more (say, 3,000 letters cut to 1,499); and a
// copy of a value of the length rows keyed on how many times a pair was cut, or an allocation
// HeadRunes makes and does not return, each keyed too on lengths inside a range, away from its
// edges and from the lengths the ninth and tenth rows hold (from 1,100 to 1,900 bytes, say), or
// keyed on how many times a pair was cut and on a place of one run or the character once that the
// ninth row does not hold, at a length of 8,000 bytes or more, where each row that holds the other
// places cuts each pair once (say, from a pair's second call on, the character once after 80 to
// 100 letters, in values of 8,001 bytes only). The last rows hold values longer than five bytes,
// each cut to a slice with no copy, up to 32,000 characters, the most an embedding input keeps.
// Among them, a cut after characters outside the BMP fails a count of UTF-16 units, and a cut
// before a combining mark fails a count that keeps a mark with its letter.
//
// Each numbered row stops the test, naming itself, when it makes another count of comparisons,
// cuts or pairs than the one written with it, the fifth and sixth too, and the shape rows stop it
// on their count together. Inside a row, each list it makes and hands on, and each loop over a
// list it reads, stops it on its own count, naming the list or the loop: the pairs the second and
// third rows hand copiesNothing (sliced), the cuts between each pair of sets and each of the two
// loops over a linker's script in the fifth row, and each loop of the sixth. The counts the fifth
// and sixth rows read from Go's tables are written from Unicode 17.0.0's, and both rows stop
// first, naming the version, on tables of another (wantTables). Each list of inputs the test
// writes out, or builds before the rows from what it writes (joined, counted against Go's marks
// and others), stops the test too, naming the list; a list a row makes inside its own loop, as the
// third row's around is, stops it through the row's count, under the row's name. And the two copy
// helpers stop a row that gives them no pair at all. So no list of any row can be empty or short
// unseen, whether the test writes it out or a loop makes it, and a row added later that gives the
// copy helpers no pair stops the test too.
func TestHeadRunesCutsAtARuneAndCopiesNothing(t *testing.T) {
	const longest, limits = 5, 6
	// The first row builds its values of the alphabet's bytes, and the seeded row of those bytes
	// and headRunesCharacters, so a byte or a character taken out would leave each value that
	// holds it unbuilt, and both rows would still pass.
	wantCount(t, "headRunesAlphabet", len(headRunesAlphabet), 12, "bytes", "the first two rows would build their values of fewer bytes")
	wantCount(t, "headRunesCharacters", len(headRunesCharacters), 5, "characters", "the seeded row would build its values of fewer characters")
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

	t.Run("every pair agrees with the reference, and is a slice of its value where the reference is one", func(t *testing.T) {
		differ, compared := 0, 0
		for _, value := range values {
			for limit := 0; limit <= limits; limit++ {
				compared++
				got, want := HeadRunes(value, limit), headRunesReference(value, limit)
				if got == want && (!strings.HasPrefix(value, want) || isSlice(got, value, len(want))) {
					continue
				}
				if differ < 10 && got == want {
					t.Errorf("HeadRunes(%q, %d) = %q at another address, want the value's own first %d bytes", value, limit, got, len(want))
				} else if differ < 10 {
					t.Errorf("HeadRunes(%q, %d) = %q, want %q", value, limit, got, want)
				}
				differ++
			}
		}
		wantCount(t, "this row", compared, 1900171, "pairs", "it would compare fewer values with the reference, or each at fewer limits")
		if differ > 0 {
			t.Errorf("%d of %d pairs differ from the reference, or are a copy where it is a slice", differ, compared)
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
		differ, compared := 0, 0
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
				compared++
				got, want := HeadRunes(value, limit), headRunesReference(value, limit)
				slice := strings.HasPrefix(value, want)
				if slice {
					sliced = append(sliced, pair{value, limit, len(want)})
				}
				if got == want && (!slice || isSlice(got, value, len(want))) {
					continue
				}
				if differ < 10 && got == want {
					t.Errorf("HeadRunes(%q, %d) = %q at another address, want the value's own first %d bytes", value, limit, got, len(want))
				} else if differ < 10 {
					t.Errorf("HeadRunes(%q, %d) = %q, want %q", value, limit, got, want)
				}
				differ++
			}
		}
		wantCount(t, "this row", compared, 1420000, "pairs", "it would compare fewer seeded values with the reference, or each at fewer limits")
		if differ > 0 {
			t.Errorf("%d of %d seeded pairs differ from the reference, or are a copy where it is a slice", differ, compared)
		}
		// copiesNothing cuts only the pairs of sliced, so it cannot see the list short.
		wantCount(t, "sliced", len(sliced), 1167422, "pairs", "copiesNothing would cut fewer of the row's pairs 22 times")
		copiesNothing(t, sliced)
	})

	t.Run("each of six kinds of character at each place of 2,001 characters agrees with the reference, and copies nothing where its answer is a slice", func(t *testing.T) {
		const characters = 2001
		differ, cuts := 0, 0
		var sliced []pair
		kinds := []string{"\xff", "\uFFFD", "é", "界", "\U0001F54C", "\u0301"}
		wantCount(t, "kinds", len(kinds), 6, "characters", "the row would put fewer kinds of character at each place")
		for _, kind := range kinds {
			for place := range characters {
				value := strings.Repeat("a", place) + kind + strings.Repeat("a", characters-1-place)
				around := []int{place - 1, place, place + 1, place + 2, characters - 1}
				for i, limit := range around {
					if limit < 0 || slices.Contains(around[:i], limit) {
						continue
					}
					cuts++
					got, want := HeadRunes(value, limit), headRunesReference(value, limit)
					slice := strings.HasPrefix(value, want)
					if slice {
						sliced = append(sliced, pair{value, limit, len(want)})
					}
					if got == want && (!slice || isSlice(got, value, len(want))) {
						continue
					}
					if differ < 10 && got == want {
						t.Errorf("%+q at character %d, cut to %d: HeadRunes kept the right %d bytes at another address, want the value's own first bytes", kind, place, limit, len(got))
					} else if differ < 10 {
						from := min(max(0, place-5), len(got), len(want))
						t.Errorf("%+q at character %d, cut to %d: HeadRunes kept %d bytes, %.20q... from byte %d, want %d bytes, %.20q...", kind, place, limit, len(got), got[from:], from, len(want), want[from:])
					}
					differ++
				}
			}
		}
		wantCount(t, "this row", cuts, 60006, "cuts", "it would cut its values at fewer limits, or put fewer kinds of character at fewer places")
		if differ > 0 {
			t.Errorf("%d of %d cuts differ from the reference, or are a copy where it is a slice", differ, cuts)
		}
		wantCount(t, "sliced", len(sliced), 54009, "pairs", "copiesNothing would cut fewer of the row's pairs 22 times")
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
	// The fifth row checks each linker the list holds, so it cannot see the list emptied or short;
	// the fourth and fifth rows would then cut after fewer linkers and still pass.
	wantCount(t, "linkers", len(linkers), 23, "strings", "the fourth and fifth rows would cut after fewer linkers")
	// letters holds the letters of each script of Go's tables, under the script's name.
	letters := map[string][]rune{}
	for name, table := range unicode.Scripts {
		for _, r := range tableRunes(table) {
			if unicode.IsLetter(r) {
				letters[name] = append(letters[name], r)
			}
		}
	}
	// joins holds each combining mark of Go's tables and, of Unicode 18.0
	// (GraphemeBreakProperty.txt), these 107 code points that are no mark in Go's tables: the
	// spacing marks of Thai and Lao that are letters, the two joiners, the halfwidth sound marks,
	// the emoji modifiers and the tags. GB9 and GB9a keep each of them after any code point but 31
	// spacing marks of Myanmar, Tai Tham and Ahom, whose Grapheme_Cluster_Break in 18.0 is Other;
	// a cut before those leaves them out too, as before any code point. joined lists them in order.
	others := map[rune]bool{}
	for _, r := range spanRunes(0x0E33, 0x0E33, 0x0EB3, 0x0EB3, 0x200C, 0x200D, 0xFF9E, 0xFF9F, 0x1F3FB, 0x1F3FF, 0xE0020, 0xE007F) {
		others[r] = true
	}
	// joined below holds the marks of Go's tables too, so its check alone cannot see others empty or
	// short; the fifth and sixth rows would then cut before fewer of the 107 and still pass.
	wantCount(t, "others", len(others), 107, "code points", "the fifth and sixth rows would cut before fewer of them")
	joins := func(r rune) bool { return unicode.Is(unicode.M, r) || others[r] }
	var joined []rune
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if joins(r) {
			joined = append(joined, r)
		}
	}
	// The fifth and sixth rows cut before the code points of joined, so a loop that built it short,
	// or over the BMP only, would leave them uncut, and both rows would still pass. Its count is the
	// marks of Go's tables, read from unicode.M itself, and those of others that are no mark.
	marks := 0
	for _, r := range tableRunes(unicode.M) {
		if !others[r] {
			marks++
		}
	}
	wantCount(t, "joined", len(joined), marks+len(others), "code points", "the fifth and sixth rows would cut before fewer of them")

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
		wantCount(t, "befores", len(befores), 62, "strings", "the row would cut after fewer of them")
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
		wantCount(t, "this row", cuts.cuts, 70060032, "cuts", "it would cut before fewer code points, or after fewer strings")
		cuts.check("before a code point")
	})

	t.Run("a cut before a code point that a grapheme rule joins to each code point of its left side leaves it out", func(t *testing.T) {
		// GB6 to GB8: the Hangul L, V and T of Unicode 18.0 (GraphemeBreakProperty.txt), with the
		// Kirat Rai V among the V, and the syllables U+AC00 to U+D7A3, of which LV are those whose
		// distance from U+AC00 is a multiple of 28, and LVT the rest. GB12 and GB13: the regional
		// indicators, RI.
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
		// Each rule names the sets of its left side and of its right, and cuts each code point of
		// each right set after each of each left set. A pair of sets that makes no cut, as when one
		// of them holds no code point, fails the row by name, as a linker does below: the other
		// pairs' cuts would still pass it. That check cannot see a set short, or a rule or a side
		// of one taken out, so the row counts each set and the pairs of sets the rules name.
		sets := map[string][]rune{"L": leading, "V": vowels, "T": trailing, "LV": lv, "LVT": lvt, "RI": regional}
		for name, want := range map[string]int{"L": 125, "V": 100, "T": 137, "LV": 399, "LVT": 10773, "RI": 26} {
			wantCount(t, "the set "+name, len(sets[name]), want, "code points", "the row would cut between fewer of the code points these rules keep together")
		}
		rules := []struct{ left, right []string }{
			{[]string{"L"}, []string{"L", "V", "LV", "LVT"}},
			{[]string{"LV", "V"}, []string{"V", "T"}},
			{[]string{"LVT", "T"}, []string{"T"}},
			{[]string{"RI"}, []string{"RI"}},
		}
		cuts := keptCuts{t: t}
		setPairs := 0
		for _, rule := range rules {
			for _, leftSet := range rule.left {
				for _, rightSet := range rule.right {
					setPairs++
					made := cuts.cuts
					for _, left := range sets[leftSet] {
						for _, right := range sets[rightSet] {
							cuts.cut(string(left), string(right)+"a")
						}
					}
					if cuts.cuts == made {
						t.Errorf("no cut was made before a code point of %s after one of %s: one of the two holds no code point", rightSet, leftSet)
					}
				}
			}
		}
		wantCount(t, "rules", setPairs, 11, "pairs of sets", "the row would cut between the code points of fewer of the pairs of sets GB6 to GB8, GB12 and GB13 keep together")
		// The sets and the pairs of sets are counted above; the cuts between them are counted too,
		// so a rule that names another set in place of one of its own, or a loop over fewer code
		// points of a set, stops the row as well.
		wantCount(t, "the cuts between the code points of each pair of sets", cuts.cuts, 3038234, "cuts", "the row would cut between fewer of the code points these rules keep together")
		// GB9c, as Unicode 18.0 writes it, keeps a consonant after a linker and any InCB=Extend:
		// each letter of the script of a linker's consonant, the linker, then each letter of that
		// script, which holds each consonant of the script that Go's tables call a letter; and the
		// consonant, the linker and each code point of joined, which holds each InCB=Extend of Go's
		// tables, then each letter of that script. A linker that makes no cut here, since Go's tables
		// give its consonant no script with a letter, fails the row by name. Each of the two loops
		// is counted against its own inputs, so a loop that runs over fewer of them stops the row,
		// naming it and the script, though the other loop's cuts would still pass it.
		beforeLinkers := cuts.cuts
		for _, linked := range linkers {
			consonant, size := utf8.DecodeRuneInString(linked)
			made := cuts.cuts
			for name, table := range unicode.Scripts {
				if unicode.Is(table, consonant) {
					before := cuts.cuts
					for _, letter := range letters[name] {
						for _, next := range letters[name] {
							cuts.cut(string(letter)+linked[size:], string(next)+"a")
						}
					}
					wantCount(t, "the cuts after each letter of "+name+", the linker "+fmt.Sprintf("%+q", linked[size:])+", then each letter", cuts.cuts-before, len(letters[name])*len(letters[name]), "cuts", "the row would cut fewer consonants after a letter and a linker")
					before = cuts.cuts
					for _, extend := range joined {
						for _, next := range letters[name] {
							cuts.cut(linked+string(extend), string(next)+"a")
						}
					}
					wantCount(t, "the cuts after the consonant and the linker "+fmt.Sprintf("%+q", linked)+", each code point of joined, then each letter of "+name, cuts.cuts-before, len(joined)*len(letters[name]), "cuts", "the row would cut fewer consonants after a linker and an InCB=Extend")
				}
			}
			if cuts.cuts == made {
				t.Errorf("no cut was made after the linker %+q: Go's tables give its consonant %+q no script with a letter", linked[size:], consonant)
			}
		}
		// The two loops' counts above are read from Go's tables at run time, so they cannot see a
		// script's letters, or a linker's script, come out short; this count, written from Go's
		// tables of Unicode 17.0.0, can.
		wantTables(t)
		wantCount(t, "the cuts after each linker", cuts.cuts-beforeLinkers, 3597051, "cuts", "the row would cut fewer consonants after a linker")
		// GB11: each code point and a joiner, then the first pictograph.
		beforeJoiners := cuts.cuts
		for r := rune(0); r <= utf8.MaxRune; r++ {
			if utf8.ValidRune(r) {
				cuts.cut(string(r)+"\u200D", "\u00A9a")
			}
		}
		wantCount(t, "the cuts after each code point and a joiner", cuts.cuts-beforeJoiners, 1112064, "cuts", "the row would cut before the first pictograph after fewer code points")
		wantCount(t, "this row", cuts.cuts, 7747349, "cuts", "it would cut fewer pairs that a grapheme rule keeps together")
		cuts.check("before a code point that a grapheme rule joins to the one before it")
	})

	t.Run("a cut before each mark that a grapheme rule keeps after any code point, after each letter of its own script or of its 128 code points, an Inherited mark after the first letter of each script, a SignWriting mark after each SignWriting base and an emoji modifier after each emoji it modifies, leaves it out", func(t *testing.T) {
		// GB9 and GB9a keep each code point of joins after any code point but 31 (above). Each loop
		// below cuts over input sets of its own, read from Go's tables, and the row stops, naming
		// them, when a loop makes another count of cuts than the one written with it, as when one of
		// its sets holds fewer code points: the other loops' cuts would still pass it. The counts are
		// written from Go's tables of Unicode 17.0.0, so the row stops first on tables of another
		// version, naming it.
		wantTables(t)
		cuts := keptCuts{t: t}
		made := 0
		madeCuts := func(sets string, want int) {
			t.Helper()
			wantCount(t, "the cuts over "+sets, cuts.cuts-made, want, "cuts", "the row would cut fewer marks, or after fewer letters or bases")
			made = cuts.cuts
		}
		for _, name := range slices.Sorted(maps.Keys(unicode.Scripts)) {
			for _, mark := range tableRunes(unicode.Scripts[name]) {
				if joins(mark) {
					for _, letter := range letters[name] {
						cuts.cut(string(letter), string(mark)+"a")
					}
				}
			}
		}
		madeCuts("unicode.Scripts, the marks of joins in each script and the letters of each script (letters)", 667961)
		for _, mark := range joined {
			for letter := mark &^ 0x7F; letter < mark&^0x7F+0x80; letter++ {
				if unicode.IsLetter(letter) {
					cuts.cut(string(letter), string(mark)+"a")
				}
			}
		}
		madeCuts("joined and the letters among each mark's 128 code points", 114432)
		// A mark of the script Inherited takes the script of the letter before it, and Inherited holds
		// no letter: each of its code points of joins after the first letter of each script, which
		// stands for its script. SignWriting holds no letter either: each of its marks after each of
		// its code points that joins nothing, its bases. And each emoji modifier after each emoji
		// modifier base of Unicode 18.0 (emoji-data.txt), which Go's tables do not name.
		inherited := tableRunes(unicode.Inherited)
		for _, name := range slices.Sorted(maps.Keys(letters)) {
			for _, mark := range inherited {
				if joins(mark) {
					cuts.cut(string(letters[name][0]), string(mark)+"a")
				}
			}
		}
		madeCuts("inherited, the code points of joins in it, and the first letter of each script (letters)", 116964)
		signWriting := tableRunes(unicode.SignWriting)
		for _, mark := range signWriting {
			if joins(mark) {
				for _, base := range signWriting {
					if !joins(base) {
						cuts.cut(string(base), string(mark)+"a")
					}
				}
			}
		}
		madeCuts("signWriting, its marks (those of joins) and its bases (those not of joins)", 69215)
		modifierBases := spanRunes(0x261D, 0x261D, 0x26F9, 0x26F9, 0x270A, 0x270D, 0x1F385, 0x1F385, 0x1F3C2, 0x1F3C4,
			0x1F3C7, 0x1F3C7, 0x1F3CA, 0x1F3CC, 0x1F442, 0x1F443, 0x1F446, 0x1F450, 0x1F466, 0x1F478, 0x1F47C, 0x1F47C,
			0x1F481, 0x1F483, 0x1F485, 0x1F487, 0x1F48F, 0x1F48F, 0x1F491, 0x1F491, 0x1F4AA, 0x1F4AA, 0x1F574, 0x1F575,
			0x1F57A, 0x1F57A, 0x1F590, 0x1F590, 0x1F595, 0x1F596, 0x1F645, 0x1F647, 0x1F64B, 0x1F64F, 0x1F6A3, 0x1F6A3,
			0x1F6B4, 0x1F6B6, 0x1F6C0, 0x1F6C0, 0x1F6CC, 0x1F6CC, 0x1F90C, 0x1F90C, 0x1F90F, 0x1F90F, 0x1F918, 0x1F91F,
			0x1F926, 0x1F926, 0x1F930, 0x1F939, 0x1F93C, 0x1F93E, 0x1F977, 0x1F977, 0x1F9B5, 0x1F9B6, 0x1F9B8, 0x1F9B9,
			0x1F9BB, 0x1F9BB, 0x1F9CD, 0x1F9CF, 0x1F9D1, 0x1F9DD, 0x1FAC3, 0x1FAC5, 0x1FAF0, 0x1FAFA)
		// The loop's count below sees the modifiers' range short; this count names the list.
		wantCount(t, "modifierBases", len(modifierBases), 136, "code points", "the row would cut the modifiers after fewer bases")
		for _, base := range modifierBases {
			for modifier := rune(0x1F3FB); modifier <= 0x1F3FF; modifier++ {
				cuts.cut(string(base), string(modifier)+"a")
			}
		}
		wantCount(t, "the cuts of the five emoji modifiers after modifierBases", cuts.cuts-made, 680, "cuts", "the row would cut fewer modifiers, or after fewer bases")
		wantCount(t, "this row", cuts.cuts, 969252, "cuts", "it would cut fewer marks, or after fewer letters or bases")
		cuts.check("before a mark")
	})

	// wide holds é, 界 and U+1F54C, characters of two, three and four bytes, which the length rows
	// and the two rows that cut every place put among letters.
	wide := []string{"é", "界", "\U0001F54C"}
	wantCount(t, "wide", len(wide), 3, "characters", "the length rows would put characters of fewer widths among letters")
	// lengthPairs gives values at each of lengths, in bytes, each cut to its own length in characters
	// and to one short, each pair once. One is of letters. For each character of wide, the others
	// hold letters and one run of the character, or the character once. Below, few is the remainder
	// of the length's place in lengths divided by 64, so over each 64 lengths running it takes each
	// count from 0 to 63, and spread is that place times 5,062 modulo 8,191, a prime, as a share of
	// the places the character has in a value of the length. 5,062 is about 8,191 times 0.618, the
	// golden ratio's fraction, whose multiples stand far apart however few of them run, so the place
	// moves along the whole value in every row: of the values of each width that hold the character
	// once after spread letters, each twentieth of the value holds 395 to 407 of the seventh row's
	// 7,996 to 7,998, 61 to 63 of the eighth's 1,232, and 0 to 3 of the ninth's 32 to 34, whose
	// places leave no stretch of more than 6% of the value empty.
	//   - A run first, with as many letters after it as the length leaves over, and, where it leaves
	//     some, the run alone, as long as the length rounded down to the character's width. A run
	//     last, after one to four letters, as many as the length needs, and a run between one letter
	//     and one to four.
	//   - A run last, after few letters or up to three more, as many as the length needs, and a run
	//     first, before as many: a run with 0 to 66 letters on one side and none on the other.
	//   - A run of half the length last, after the other half in letters, or first, before them; a
	//     run of half the length between two quarters in letters; and two of the character between
	//     two halves of letters: a run with letters beside it up to half the length.
	//   - The character once first, last, in the middle, and after or before few letters, with the
	//     rest of the letters on its other side.
	//   - The character once after spread letters, and a run last after about as many, or first
	//     before as many, as the length's width allows.
	// Each value but the two runs between letters is a slice of a string the row makes once for its
	// character, so for each length the row makes only those two values of its own.
	lengthPairs := func(lengths []int) []pair {
		var pairs []pair
		seen := map[pair]bool{}
		add := func(value string) {
			for _, p := range cutPairs(value) {
				if !seen[p] {
					seen[p] = true
					pairs = append(pairs, p)
				}
			}
		}
		to := slices.Max(lengths)
		letters := strings.Repeat("a", to)
		for _, n := range lengths {
			add(letters[:n])
		}
		for _, character := range wide {
			width := len(character)
			run := strings.Repeat(character, to/width)
			// first[over] is the run then over letters, last[over] is over+1 letters then the run;
			// once is the character between two runs of letters, lettersRun and runLetters the run
			// after and before letters, and twice two of the character between two runs of letters.
			first, last := make([]string, width), make([]string, width)
			for over := range width {
				first[over] = run + letters[:over]
				last[over] = letters[:over+1] + run
			}
			once := letters + character + letters
			lettersRun, runLetters, twice := letters+run, run+letters, letters+run[:2*width]+letters
			for i, n := range lengths {
				end := first[n%width]
				add(end[len(end)-n:])
				if n < width+2 {
					continue
				}
				add(last[(n-1)%width][:n])
				after := (n-2)%width + 1
				add("a" + run[:n-1-after] + letters[:after])
				if n%width != 0 {
					add(run[:n-n%width])
				}
				add(once[to : to+n])
				add(once[to+width-n : to+width])
				before := (n - width) / 2
				add(once[to-before : to-before+n])
				few := i % 64
				if few <= n-width {
					add(once[to-few : to-few+n])
					add(once[to+width+few-n : to+width+few])
				}
				if few += (n - few) % width; few <= n-2*width {
					add(lettersRun[to-few : to-few+n])
					add(runLetters[len(run)-(n-few) : len(run)-(n-few)+n])
				}
				// spread takes the place along the whole value as the lengths run, so the character
				// once and a run at either end stand away from the ends and the middle too.
				spread := i * 5062 % 8191 * (n - width + 1) / 8191
				add(once[to-spread : to-spread+n])
				if spread += (n - spread) % width; spread <= n-2*width {
					add(lettersRun[to-spread : to-spread+n])
					add(runLetters[len(run)-(n-spread) : len(run)-(n-spread)+n])
				}
				if n >= 2*width+2 {
					half := n / 2 / width * width
					add(lettersRun[to-(n-half) : to+half])
					add(runLetters[len(run)-half : len(run)-half+n])
					quarter := n / 4
					add(letters[:quarter] + run[:half] + letters[:n-quarter-half])
					around := (n - 2*width) / 2
					add(twice[to-around : to-around+n])
				}
			}
		}
		return pairs
	}
	var short, long []int
	for n := 0; n <= 8001; n++ {
		short = append(short, n)
	}
	// The eighth row starts one step past the seventh's last length, so no pair is in both.
	for n := 8001 + 32; n <= 32001; n += 32 {
		long = append(long, n)
	}
	for n := 32001 + 199; n <= 128001; n += 199 {
		long = append(long, n)
	}
	wantCount(t, "short", len(short), 8002, "lengths", "the seventh row would cut values of fewer lengths")
	wantCount(t, "long", len(long), 1232, "lengths", "the eighth row would cut values of fewer lengths")
	t.Run("a value of letters, or of a run or one of a character of two, three or four bytes first, last or between letters or alone, of each length up to 8,001 bytes, unchanged or cut one short, copies nothing", func(t *testing.T) {
		pairs := lengthPairs(short)
		wantCount(t, "this row", len(pairs), 822685, "pairs", "it would cut fewer values, or each to fewer limits")
		answersSlices(t, pairs)
	})
	t.Run("a value of letters, or of a run or one of a character of two, three or four bytes first, last or between letters or alone, of every 32nd length from 8,033 to 32,001 bytes and of every 199th to 128,001, unchanged or cut one short, copies nothing", func(t *testing.T) {
		pairs := lengthPairs(long)
		wantCount(t, "this row", len(pairs), 133138, "pairs", "it would cut fewer values, or each to fewer limits")
		answersSlices(t, pairs)
	})
	// The two rows above cut each pair once. This one cuts the values of each kind they hold 22
	// times, at each edge of the ranges of lengths that the rows of this test hold, and one byte each
	// side of it: 5 and 6, where the shape rows end and the seeded row starts (4 to 7); 64 and 65,
	// where the seeded row ends (63 to 66, with 67 and 68 for each remainder by two, three and four);
	// 2,001 and 2,004, the place row's (2,000 to 2,005); 8,001, where the seventh row ends (8,000 to
	// 8,002); 8,033, where the eighth starts (8,032 to 8,034); 32,001, where its step changes (32,000
	// to 32,002); and 127,919, its last (127,918 to 127,920). Inside the seventh row's range it cuts
	// 500 and 1,000 bytes too, and one byte each side (499 to 501, 999 to 1,001), where a key on a
	// range of round lengths starts or ends. So a copy keyed on how many times a pair was cut fails
	// it when its key reaches one of these lengths and a value lengthPairs gives at it, and so does
	// an allocation HeadRunes does not return. The next row cuts every place of one run or the
	// character once 22 times at these lengths up to 2,005 bytes. From 8,000 bytes on, a copy keyed
	// on how many times a pair was cut and on a place of one run or the character once that this row
	// does not hold at its length can pass, since each other row that holds a value there, the
	// seventh, the eighth, the eleventh or the twelfth, cuts each pair once (say, from a pair's
	// second call on, the character once after 80 to 100 letters, in values of 8,001 bytes only).
	edges := []int{4, 5, 6, 7, 63, 64, 65, 66, 67, 68, 499, 500, 501, 999, 1000, 1001, 2000, 2001, 2002, 2003, 2004, 2005, 8000, 8001, 8002, 8032, 8033, 8034, 32000, 32001, 32002, 127918, 127919, 127920}
	wantCount(t, "edges", len(edges), 34, "lengths", "the ninth row would cut values of fewer lengths 22 times")
	t.Run("each kind of value of the two rows above, at each edge of the lengths the rows hold and one byte each side of it (4 to 7, 63 to 68, 2,000 to 2,005, 8,000 to 8,002, 8,032 to 8,034, 32,000 to 32,002 and 127,918 to 127,920 bytes) and at 499 to 501 and 999 to 1,001 bytes, unchanged or cut one short, copies nothing on each of 22 calls", func(t *testing.T) {
		pairs := lengthPairs(edges)
		wantCount(t, "this row", len(pairs), 3234, "pairs", "it would cut fewer values, or each to fewer limits")
		copiesNothing(t, pairs)
	})
	// The length rows put the character once, and a run beside letters, at a few places of each
	// length. The next three put them at every place: everyPlacePairs gives, for each character of
	// wide, each value of each of lengths, in bytes, that holds letters and the character once, at
	// each of its places, or letters and one run of two characters or more with each count of
	// letters before it and none after, or after it and none before, each cut to its own length in
	// characters and to one short. Each of those values is a slice of a string it makes once for
	// its character, so it makes no value of its own.
	//
	// No argument from HeadRunes' code can stand in for cutting each of those values at each
	// length. HeadRunes converts only a kept part that holds a byte that is not UTF-8, so for a
	// value of letters and a wider character it answers a slice at every length and every place:
	// its code compares no length and no place, and so gives no class of lengths or places that one
	// cut could stand for. Nor of limits or characters: it counts the limit down to nought and reads
	// each character only for a byte that is not UTF-8, so a cut at one limit, or of a value that
	// holds one character, stands for no other, and every limit of every value of letters up to
	// 127,919 bytes would read about 3.5 × 10^14 bytes. A change that copies can compare any length,
	// place, limit or character, and a copy keyed on one value fails only a cut of that value, so
	// whatever values a test cuts, a copy keyed on another passes it. Cutting a value reads it whole:
	// the values of one length come to about ten times the square of the length in bytes, about
	// 1.7 × 10^11 at 127,919 bytes, so every place at every length up to 127,919 would read about
	// 7 × 10^15. So the first of the three rows cuts its pairs 22 times at the ninth row's lengths up
	// to 2,005 bytes, the 2,000 characters a document edit reads back among them; the second once, at
	// 5,000 bytes, at 8,001, where the seventh row ends, and at 8,033, where the eighth starts; and
	// the third once at 127,919, where the eighth ends. The other lengths are left to the length
	// rows' few places, and the other limits to the thirteenth and fourteenth rows' middle and
	// callers' limits.
	everyPlacePairs := func(lengths []int) []pair {
		var pairs []pair
		add := func(value string) {
			cut := cutPairs(value)
			pairs = append(pairs, cut[:]...)
		}
		to := slices.Max(lengths)
		letters := strings.Repeat("a", to)
		for _, character := range wide {
			width := len(character)
			run := strings.Repeat(character, to/width)
			once, lettersRun, runLetters := letters+character+letters, letters+run, run+letters
			for _, n := range lengths {
				for place := 0; place <= n-width; place++ {
					add(once[to-place : to-place+n])
				}
				// count letters then the run, and the run then count letters; with no letter the
				// two are one value.
				for count := n % width; count <= n-2*width; count += width {
					add(lettersRun[to-count : to-count+n])
					if count > 0 {
						add(runLetters[len(run)-(n-count) : len(run)-(n-count)+n])
					}
				}
			}
		}
		return pairs
	}
	var everyPlaceCalls []int
	for _, n := range edges {
		if n <= 2005 {
			everyPlaceCalls = append(everyPlaceCalls, n)
		}
	}
	everyPlaceOnce := []int{5000, 8001, 8033}
	wantCount(t, "everyPlaceCalls", len(everyPlaceCalls), 22, "lengths", "the next row would cut every place at fewer lengths")
	wantCount(t, "everyPlaceOnce", len(everyPlaceOnce), 3, "lengths", "the row after it would cut every place at fewer lengths")
	t.Run("the character once at each place, or a run beside each count of letters on one side of it, of a character of two, three or four bytes, at 4 to 7, 63 to 68, 499 to 501, 999 to 1,001 and 2,000 to 2,005 bytes, unchanged or cut one short, copies nothing on each of 22 calls", func(t *testing.T) {
		pairs := everyPlacePairs(everyPlaceCalls)
		wantCount(t, "this row", len(pairs), 174286, "pairs", "it would cut fewer values, or each to fewer limits")
		copiesNothing(t, pairs)
	})
	t.Run("the character once at each place, or a run beside each count of letters on one side of it, of a character of two, three or four bytes, at 5,000, 8,001 and 8,033 bytes, unchanged or cut one short, copies nothing", func(t *testing.T) {
		pairs := everyPlacePairs(everyPlaceOnce)
		wantCount(t, "this row", len(pairs), 217262, "pairs", "it would cut fewer values, or each to fewer limits")
		answersSlices(t, pairs)
	})
	t.Run("the character once at each place, or a run beside each count of letters on one side of it, of a character of two, three or four bytes, at 127,919 bytes, where the eighth row ends, unchanged or cut one short, copies nothing", func(t *testing.T) {
		pairs := everyPlacePairs([]int{127919})
		wantCount(t, "this row", len(pairs), 1321798, "pairs", "it would cut fewer values, or each to fewer limits")
		answersSlices(t, pairs)
	})
	// prose is text of the kind HeadRunes' callers cut: a document's blocks, a message, an excerpt.
	// The rows above build the values whose answers they check for a copy of the letter a, the
	// alphabet's bytes and a few wider characters, so a copy keyed on another byte or character in a
	// longer value passes them. prose holds each printable ASCII byte, a tab, a carriage return and a
	// newline; letters with accents, Greek, Cyrillic, Chinese, Japanese and Korean; a letter and a
	// combining mark; and characters outside the BMP: an emoji, a flag, a musical symbol and a
	// Chinese character of the second plane.
	const prose = "The quick brown fox jumps over the lazy dog. THE QUICK BROWN FOX JUMPS OVER THE LAZY DOG! " +
		"Call 0123456789 at 9:30; ask \"Is 2 + 2 = 4?\" (yes) [ok] {fine} <tag> ~/`code` #1 $5 50% ^up " +
		"*bold* _it_ |a| \\b /c @d & 'e', well-known.\tA tab, then a new line:\r\n" +
		"El niño comió jalapeño über die Straße, garçon, smørrebrød; Ελληνικά, русский. " +
		"中文的文字，日本語のかな、한국어 글자. Cafe\u0301 au lait 😀 🇫🇷 𝄞 𠜎.\n"
	held := map[rune]bool{}
	for _, r := range prose {
		held[r] = true
	}
	for _, r := range append(spanRunes(' ', '~'), '\t', '\r', '\n') {
		if !held[r] {
			t.Fatalf("prose holds no %+q, so the prose row would cut no value that holds it", r)
		}
	}
	// A character taken out of prose would leave each value that held it unbuilt, and the row would
	// still pass.
	wantCount(t, "prose", len(held), 140, "characters", "the prose row would put fewer characters in its values")
	// callerLimits are the limits HeadRunes' callers pass, each a constant of its caller: 120 and
	// 160, an outbox event's detail and summary; 160, a message's reply preview; 240, a reference's
	// excerpt; 480, a block's excerpt; 1,200, an item's window; 1,967, where a read-back of one block
	// of 2,001 to 9,999 characters cuts it; 2,000, a check run's output; and 32,000, an embedding
	// input.
	callerLimits := []int{120, 160, 240, 480, 1200, 1967, 2000, 32000}
	wantCount(t, "callerLimits", len(callerLimits), 8, "limits", "the rows below would cut their values at fewer of the limits callers pass")
	// callerPairs gives, for each of lengths, in bytes, a value of unit over and over, cut in its
	// middle and at each of callerLimits below its own count of characters less one, and, when
	// whole, to its own length and to one character short: each pair once. The value starts at a
	// place of unit that the length's place in lengths, times 5,062 modulo 8,191, spreads over it,
	// as the length rows spread theirs, so each character of unit stands at many places of the
	// values, and it starts and ends at the whole characters nearest those bytes. A read-back cuts
	// its last block to the room it has left, any count up to 2,000, and the middle cuts reach each
	// such count, in a value of twice it.
	callerPairs := func(unit string, lengths []int, whole bool) []pair {
		filler := strings.Repeat(unit, (slices.Max(lengths)+2*len(unit))/len(unit))
		var pairs []pair
		seen := map[[2]int]bool{}
		for i, n := range lengths {
			from := i * 5062 % 8191 * len(unit) / 8191
			for !utf8.RuneStart(filler[from]) {
				from--
			}
			to := from + n
			for !utf8.RuneStart(filler[to]) {
				to--
			}
			if seen[[2]int{from, to}] {
				continue
			}
			seen[[2]int{from, to}] = true
			value := filler[from:to]
			characters := utf8.RuneCountInString(value)
			if whole {
				cut := cutPairs(value)
				pairs = append(pairs, cut[0])
				if characters > 0 {
					pairs = append(pairs, cut[1])
				}
			}
			middle := characters / 2
			if middle < characters-1 {
				pairs = append(pairs, cutAt(value, middle))
			}
			for _, limit := range callerLimits {
				if limit != middle && limit < characters-1 {
					pairs = append(pairs, cutAt(value, limit))
				}
			}
		}
		return pairs
	}
	t.Run("a value of prose, with each printable ASCII byte and letters of six scripts, outside the BMP too, of each length the seventh and eighth rows hold, unchanged, cut one short, in its middle and at each limit a caller of HeadRunes passes, copies nothing on each of 22 calls", func(t *testing.T) {
		pairs := callerPairs(prose, slices.Concat(short, long), true)
		wantCount(t, "this row", len(pairs), 85348, "pairs", "it would cut fewer values, or each to fewer limits")
		copiesNothing(t, pairs)
	})
	// The length rows cut each value of letters to its own length and one short only, so a copy of
	// a cut in the middle of a value of letters passed them; this row cuts those values where the
	// callers cut them.
	t.Run("a value of letters cut in its middle and at each limit a caller of HeadRunes passes, of each length the seventh and eighth rows hold, copies nothing on each of 22 calls", func(t *testing.T) {
		pairs := callerPairs("a", slices.Concat(short, long), false)
		wantCount(t, "this row", len(pairs), 68156, "pairs", "it would cut fewer values, or each to fewer limits")
		copiesNothing(t, pairs)
	})

	shapes := map[string][]pair{}
	for _, value := range values {
		for limit := 0; limit <= limits; limit++ {
			if want := headRunesReference(value, limit); strings.HasPrefix(value, want) {
				shape := shapeOf(value, limit)
				shapes[shape] = append(shapes[shape], pair{value, limit, len(want)})
			}
		}
	}
	// shapeOf names 19 shapes of the first row's pairs whose answer is a slice; a change to it or to
	// the values that left one out would run one row fewer and still pass.
	wantCount(t, "shapes", len(shapes), 19, "shapes", "fewer shape rows would run")
	shaped := 0
	for _, pairs := range shapes {
		shaped += len(pairs)
	}
	wantCount(t, "the shape rows", shaped, 1016173, "pairs", "they would cut fewer of the first row's pairs")
	for _, shape := range slices.Sorted(maps.Keys(shapes)) {
		pairs := shapes[shape]
		t.Run(shape+" copies nothing", func(t *testing.T) { copiesNothing(t, pairs) })
	}

	kept := []struct {
		name, value string
		limit       int
		want        string
	}{
		{name: "characters outside the BMP are cut at the limit", value: strings.Repeat("𠀀", 10), limit: 4, want: strings.Repeat("𠀀", 4)},
		{name: "2,001 Chinese characters are cut at 1,967", value: strings.Repeat("界", 2001), limit: 1967, want: strings.Repeat("界", 1967)},
		{name: "100 pairs of a letter and U+FFFD are cut at 50", value: strings.Repeat("a\uFFFD", 100), limit: 50, want: strings.Repeat("a\uFFFD", 25)},
		{name: "a byte that is not UTF-8 past the cut is left out", value: strings.Repeat("界", 10) + "\xff", limit: 5, want: strings.Repeat("界", 5)},
		{name: "a combining mark after the cut is left out", value: strings.Repeat("a\u0301", 10), limit: 3, want: "a\u0301a"},
		{name: "2,001 Chinese characters are returned unchanged", value: strings.Repeat("界", 2001), limit: 2001, want: strings.Repeat("界", 2001)},
		{name: "32,000 letters are returned unchanged", value: strings.Repeat("a", 32000), limit: 32000, want: strings.Repeat("a", 32000)},
		{name: "32,000 Chinese characters are returned unchanged", value: strings.Repeat("界", 32000), limit: 32000, want: strings.Repeat("界", 32000)},
		{name: "32,001 letters are cut at 32,000", value: strings.Repeat("a", 32001), limit: 32000, want: strings.Repeat("a", 32000)},
		{name: "32,001 Chinese characters are cut at 32,000", value: strings.Repeat("界", 32001), limit: 32000, want: strings.Repeat("界", 32000)},
	}
	wantCount(t, "kept", len(kept), 10, "rows", "fewer of the rows below would run")
	for _, row := range kept {
		t.Run(row.name, func(t *testing.T) {
			if !strings.HasPrefix(row.value, row.want) {
				t.Fatalf("the row wants %.40q, which is not the first bytes of its value", row.want)
			}
			copiesNothing(t, []pair{{row.value, row.limit, len(row.want)}})
		})
	}
}
