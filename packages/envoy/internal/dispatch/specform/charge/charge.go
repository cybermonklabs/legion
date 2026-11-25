// Package charge holds the texts that one judgment of the ways out of a text's findings reads (the
// judge of package specform), and it charges each reading of a text to that text. A text is a
// sentence of the judged text, a sentence of a title or the address of a link or an image, and the
// judgment names it by its Unit, an index, in each map, each key and each walk. Its bytes, and what
// the judgment keeps of each place of it in the judged text, are in Texts, outside the judge's
// package, and the one way to read them is Read, which charges the reading first and gives the text
// only where the charge lets it. The judge's fields and keys hold no text of a sentence but the
// words of each finding and of its ways out, which a rule hands the judge with the finding; the
// detail of each finding that a reading gives (the key of the findings that the judge compares),
// which quotes the finding's words, or for a finding of the rule ste100-dangling-conjunction the
// last line of its list item; and the words of the first place of each member of a synonym group,
// one for each member of rules.json at most. Beside them it holds what the judgment is told of the
// system and its people. For a finding of the rule instructions the words are its whole sentence:
// the judge hashes them in each map keyed by the finding a fixed number of times for that finding,
// and reads them only as the words that a way out changes, once for each place where it writes the
// way out, as it reads any finding's words. A detail it keeps as a reading gives it, so each comes
// from one charged reading of a text or from one text that a way out writes. So the judgment reads
// each text a bounded number of times, however many findings read it, and its work stays in
// proportion to the judged text. The judgments of the texts of one document can share a Budget
// (Share), which each reading of any of them takes the bytes of its text from, so their work
// together also stays within a bound that no document passes, whatever its size.
//
// What Read gives can be kept in a variable of the code that read it, as any value of Go can; a
// copy of a text in a field of the judge's own, read again later, would be read outside the charge.
// The judge keeps no such copy, and the tests of package specform refuse a field of the judge that
// can hold text but those above, wherever the walk that would read it sits: they walk the type of
// each field of the judge, into the values of an Index and the elements of a channel, and name each
// string, slice, array or channel of bytes or runes and interface outside the fields that hold the
// text above (TestTheJudgeKeepsNoTextOfASentenceInAField). No type can tell a copy of what a
// reading gave as counts or flags, such as whether each sentence runs on into the next, from what
// the judge keeps of the split of its text, nor show what a function value holds. For a copy read
// in the look for the sentences where a finding's words stand (holding) or in the walk of the
// sentences between two that a way out changes (changedRuns), the work table of package specform
// catches its walk for each finding: its row of many findings of one claim whose words one sentence
// after them holds in code font, and its row of many findings whose two places a run of open quoted
// sentences joins.
package charge

import "fmt"

// Answer is whether a charge let a reading go ahead (Read), or whether each charge that a walk asked
// let it: a walk of the judge that reads through Read hands its answer on as an Answer. Each call of
// package specform whose results hold one binds it to a name, or hands it on whole as its own
// results, which the tests of that package hold for each call by the type alone
// (TestEachReadingOfATextKeepsTheAnswerOfItsCharge). No rule over the form of the code can hold what
// the code then does with an answer it bound (an answer read in a test that is always true acts on
// nothing), so those tests hold that by what the judgment answers: each reading of a judgment of
// their texts, refused in turn (Refuse), ends in the refusal of the way out that asked it or changes
// no answer, and each such call is on the way to a refused reading
// (TestEachRefusedReadingRefusesTheWayOutThatAskedIt). So no code of the judge that those texts
// reach reads on past a charge that refused it as though the charge had said yes.
type Answer bool

// Unit names one text of a judgment: a sentence of the judged text, a sentence of a title of it
// (title), or the address of a link or an image (address), as Texts.Name gives it, or no text (a
// place that the judge did not find, None). A Unit holds no byte of its text.
type Unit struct {
	// index is one more than the place of the text in its Texts, so that the zero Unit names none.
	index          int32
	address, title bool
}

// None is the unit of a place that names no text, in a title where title.
func None(title bool) Unit {
	return Unit{title: title}
}

// Named reports whether u names a text.
func (u Unit) Named() bool {
	return u.index != 0
}

// Address reports whether u names the address of a link or an image.
func (u Unit) Address() bool {
	return u.address
}

// Title reports whether u names a sentence of a title, or no text in a title.
func (u Unit) Title() bool {
	return u.title
}

// Texts is the texts of one judgment, each under its Unit, with K for each place where it stands in
// the judged text (Keep), and the charge of each: how many times the judgment has read each text,
// for a finding that stands in it (own) and for one that does not, apart. Each count stays within
// limit: a reading past it is refused (Read), and one for a finding that stands in the text stops
// the program where loud is set, as a walk that breaks the bounds that the limit is derived from.
// A reading within the limit is refused too where refuse says so (Refuse), and where the Budget
// that it shares has no bytes left for it (Share).
type Texts[K any] struct {
	limit   int
	loud    bool
	refuse  func() bool
	budget  *Budget
	entries []entry[K]
	named   map[name]int32
}

type entry[K any] struct {
	text       string
	kept       []K
	own, other int
}

// name is a text as Name knows it: two sentences of the same words are one text, so that a
// judgment reads them once, and a text of a title or an address is another text than a sentence of
// the same words.
type name struct {
	text           string
	address, title bool
}

// New is the texts of one judgment, whose readings of each text stay within limit (Texts).
func New[K any](limit int, loud bool) *Texts[K] {
	return &Texts[K]{limit: limit, loud: loud, named: map[name]int32{}}
}

// Refuse makes Read ask refuse before each reading that the count lets, and refuse each that refuse
// says yes to, as it refuses one past the limit but with no loud stop and nothing counted; nil asks
// nothing. The tests of package specform refuse each reading of a judgment in turn, so that each
// refusal is seen to end in the refusal of the way out that asked it (Answer).
func (t *Texts[K]) Refuse(refuse func() bool) {
	t.refuse = refuse
}

// Share makes each reading that Read gives take its cost from budget (Budget.take), and Read refuse
// a reading that budget has no bytes left for, as it refuses one past the limit but with no loud
// stop and nothing counted. The judgments of the texts of one document share one budget; nil takes
// nothing.
func (t *Texts[K]) Share(budget *Budget) {
	t.budget = budget
}

// Spent reports whether the budget that t shares refused a reading (Budget.Spent).
func (t *Texts[K]) Spent() bool {
	return t.budget.Spent()
}

// Budget is how many bytes the readings of the judgments that share it may still read together.
// Each reading costs the bytes of its text and ReadingCost more, for the work of a reading that
// its bytes do not count. Once a reading costs more than is left, the budget is spent, and it
// refuses each reading after it, however small, so the findings that a judgment reads the ways out
// of are the first that ask, in the order of the judgment.
type Budget struct {
	left  int
	spent bool
}

// ReadingCost is what one reading costs beside the bytes of its text.
const ReadingCost = 64

// NewBudget is a budget of bytes.
func NewBudget(bytes int) *Budget {
	return &Budget{left: bytes}
}

// Spent reports whether b refused a reading; a nil budget refuses none.
func (b *Budget) Spent() bool {
	return b != nil && b.spent
}

// take takes the cost of a reading of a text of size bytes from b, and reports whether b let it; a
// nil budget lets each.
func (b *Budget) take(size int) bool {
	if b == nil {
		return true
	}
	if cost := size + ReadingCost; b.spent || cost > b.left {
		b.spent = true
		return false
	}
	b.left -= size + ReadingCost
	return true
}

// Name is the unit of text, a sentence of the judged text, of a title (title) or an address of a
// link or an image (address): the same unit for each sentence of the same words and kind. It reads
// text once, to know it, as a caller names each place of a text once (the judge's split of a text,
// of a title, and each address and each sentence of a procedure once in each run of the rules), and
// charges nothing.
func (t *Texts[K]) Name(text string, address, title bool) Unit {
	key := name{text, address, title}
	index, ok := t.named[key]
	if !ok {
		t.entries = append(t.entries, entry[K]{text: text})
		index = int32(len(t.entries))
		t.named[key] = index
	}
	return Unit{index: index, address: address, title: title}
}

// Keep adds kept, what the judgment keeps of one place of u in the judged text, to the places of u,
// and is its place among them, the index of kept in what Read gives. It charges nothing.
func (t *Texts[K]) Keep(u Unit, kept K) int {
	at := &t.entries[u.index-1]
	at.kept = append(at.kept, kept)
	return len(at.kept) - 1
}

// Read is one reading of the text that u names, for a finding that stands in it (own) or for one
// that does not: where the text's count of such readings is under its limit, Refuse's function
// does not refuse it and the budget that t shares lets it (Share), it counts the reading, takes its
// cost from the budget, and gives the text and what the judgment keeps of each of its places, in
// the order of Keep, with the answer yes (ok true); elsewhere it counts nothing
// and gives nothing, with the answer no, so the reader reads nothing of it. A reading past the limit
// for a finding that stands in the text stops the program where loud is set. A unit that names no
// text gives nothing. The kept values may be changed in place, as the judge's one reading of each
// text for no finding fills in what it reads of each place.
func (t *Texts[K]) Read(u Unit, own bool) (text string, kept []K, ok Answer) {
	if !u.Named() {
		return "", nil, false
	}
	at := &t.entries[u.index-1]
	count := &at.other
	if own {
		count = &at.own
	}
	if *count >= t.limit {
		if own && t.loud {
			panic(fmt.Sprintf("charge: a judgment reads the text %.80q once more for the findings that stand in it, past its limit of %d: a walk breaks the bounds that the limit is derived from",
				at.text, t.limit))
		}
		return "", nil, false
	}
	if t.refuse != nil && t.refuse() {
		return "", nil, false
	}
	if !t.budget.take(len(at.text)) {
		return "", nil, false
	}
	*count++
	return at.text, at.kept, true
}

// Index is values under a text key, such as each word of the judged text under its bare form: one
// reads only the values of a key that it holds (Get), and no walk can read the keys. So a lookup
// reads its own key, not the texts that the keys came from. Get gives the values with no charge, so
// a value holds no text of a sentence, which the tests of package specform hold for each Index of
// the judge by its type.
type Index[V any] struct {
	values map[string][]V
}

// Put adds value under key.
func (x *Index[V]) Put(key string, value V) {
	if x.values == nil {
		x.values = map[string][]V{}
	}
	x.values[key] = append(x.values[key], value)
}

// Get is the values under key, in the order of Put.
func (x *Index[V]) Get(key string) []V {
	return x.values[key]
}
