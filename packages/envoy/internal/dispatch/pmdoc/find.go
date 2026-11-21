package pmdoc

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/reearth/ygo/crdt"
)

// Range is a half-open ProseMirror position range.
type Range struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// ErrTargetNotFound is the text an agent reads when a quote, heading or block anchor does not
// resolve, so it names no internal package.
var ErrTargetNotFound = errors.New("target not found")
var ErrTargetSpansBlocks = errors.New("target spans textblocks")

// ErrMarkNotAllowed is the refusal of a mark on text that stands where the schema allows no mark of
// its type: an ask's in a code block, or any anchor's in front matter. The refusal is a
// *MarkNotAllowedError, which is ErrSchema too, so a caller that only knows ErrSchema still sees
// it, and one that wrote the text itself can tell it apart from a fault of the document or the
// server.
var ErrMarkNotAllowed = errors.New("mark not allowed on this text")

// MarkNotAllowedError is MarkRange's refusal of Mark on text in Textblock, the textblock holding the
// text: "code_block", or "" for text no paragraph, heading or code block holds.
type MarkNotAllowedError struct {
	Mark      string
	Textblock string
	// At is where the text run holding the refused text begins in the document - its block's whole
	// text, not where the refused range starts. A code block or the front matter holds its text as
	// one run, so two refusals with the same Textblock and At are one place, and two with different
	// At are two places of one kind.
	At int
}

func (e *MarkNotAllowedError) Error() string {
	return fmt.Sprintf("%s: mark %q is not allowed in %s", ErrSchema, e.Mark, e.Place())
}

// Is reports err as ErrMarkNotAllowed and as ErrSchema.
func (e *MarkNotAllowedError) Is(target error) bool {
	return target == ErrMarkNotAllowed || target == ErrSchema
}

// Place names, for a person, where the refused text stands: "a code block", or "the front matter"
// for text no textblock holds, since in a document inside the schema only front matter holds text
// outside a paragraph, heading or code block.
func (e *MarkNotAllowedError) Place() string {
	singular, _ := e.place()
	return singular
}

// PlacePlural names several places of the kind the refused text stands in, for a count to stand
// before: "code blocks". It is empty where a document inside the schema holds one place of the
// kind at most - the front matter - so a caller counting the places a quote landed in names that
// one as Place names it however often the quote landed there.
func (e *MarkNotAllowedError) PlacePlural() string {
	_, plural := e.place()
	return plural
}

// place is where the refused text stands, as a person says it of one such place and of several,
// held as one mapping so the singular and the plural never name different things.
func (e *MarkNotAllowedError) place() (singular, plural string) {
	switch e.Textblock {
	case "":
		return "the front matter", ""
	case "code_block":
		return "a code block", "code blocks"
	}
	return e.Textblock, e.Textblock + "s"
}

// ErrQuoteNotFound reports a quote miss with up to three closest rendered textblocks, in
// document order among equals, and how many places the quote did match — nonzero only when an
// occurrence was out of range. It unwraps ErrTargetNotFound so callers can preserve their
// existing miss handling.
type ErrQuoteNotFound struct {
	Nearest []string
	Matches int
}

func (e *ErrQuoteNotFound) Error() string {
	return fmt.Sprintf("%s; nearest blocks: %s", ErrTargetNotFound, QuoteBlocks(e.Nearest))
}

func (e *ErrQuoteNotFound) Unwrap() error { return ErrTargetNotFound }

// QuoteBlocks renders nearest-block candidates as `"a" | "b" | "c"` for error text.
func QuoteBlocks(blocks []string) string {
	quoted := make([]string, len(blocks))
	for index, block := range blocks {
		quoted[index] = fmt.Sprintf("%q", block)
	}
	return strings.Join(quoted, " | ")
}

// ErrHeadingNotFound reports a `heading:` anchor miss with the anchor text and the document's
// closest headings. Heading anchors match the whole heading text exactly: a prefix of a longer
// heading is a miss, because prefix matching would make every anchor ambiguous.
type ErrHeadingNotFound struct {
	Title   string
	Nearest []string
}

func (e *ErrHeadingNotFound) Error() string {
	if len(e.Nearest) == 0 {
		return fmt.Sprintf("heading anchor %q not found; the document has no headings", e.Title)
	}
	return fmt.Sprintf(
		"heading anchor %q not found; a heading anchor matches the whole heading text exactly; nearest headings: %s",
		e.Title, QuoteBlocks(e.Nearest),
	)
}

func (e *ErrHeadingNotFound) Unwrap() error { return ErrTargetNotFound }

// nearestBlockCount is how many candidate blocks a quote miss names.
const nearestBlockCount = 3

// atxHeadingMarker is the `# ` … `###### ` prefix a quote may carry to mean "the heading
// whose text follows"; the level is not matched, only the heading-ness.
var atxHeadingMarker = regexp.MustCompile(`^#{1,6} +`)

// Candidate gives one matching range and enough surrounding document text to
// disambiguate it.
type Candidate struct {
	Range
	Context string `json:"context"`
}

// ErrTargetAmbiguous reports a quote that needs an occurrence or position hint. Target is the
// text that matched in several places, so the reader knows which of its quotes to narrow.
type ErrTargetAmbiguous struct {
	Target     string
	Candidates []Candidate
}

func (e *ErrTargetAmbiguous) Error() string {
	return fmt.Sprintf(
		"%q matches %d places; pass a zero-based occurrence, or quote more of the surrounding text",
		e.Target, len(e.Candidates),
	)
}

// FindQuote finds quote in the document text and returns its ProseMirror
// range. occurrence is zero-based. When several exact matches exist, near
// chooses the range whose start is nearest the ProseMirror position hint.
func FindQuote(doc *Node, quote string, occurrence *int, near *int) (Range, error) {
	return NewQuoteFinder(doc).Find(quote, occurrence, near)
}

// QuoteFinder answers FindQuote for many quotes over one document. It flattens the document's
// text once, at its first match (Find or NearestFirst), and every later match reuses that text and
// the forms derived from it, so a write that resolves many quotes against one tree walks it once
// rather than once a quote. A finder from NewQuoteFinder validates the document there too, at that
// first match; one from ValidDocument.QuoteFinder holds a tree validated already and never
// validates it again. The document must not change while the finder is in use.
type QuoteFinder struct {
	doc *Node
	// valid says the document was validated before the finder was made (ValidDocument.QuoteFinder),
	// so the first match of a quote flattens its text without validating it again.
	valid    bool
	prepared bool
	err      error
	text     flattenedText
	// inOneTextblock says the finder resolves a document edit's quotes (ValidDocument.EditQuoteFinder),
	// where only a match inside one textblock is a match (cascade), and textblocks is the content
	// range of each textblock of the document, in document order, which such a finder reads each
	// match against, built at its first use (insideOneTextblock).
	inOneTextblock bool
	textblocks     []Range
	textblocksDone bool
}

// NewQuoteFinder returns a finder over doc. It reads nothing of doc until its first Find.
func NewQuoteFinder(doc *Node) *QuoteFinder {
	return &QuoteFinder{doc: doc}
}

// QuoteFinder is a finder over d's tree whose first match does not validate it again; every answer
// is NewQuoteFinder's for the same tree. The zero ValidDocument holds no tree, so its finder
// answers FindQuote's error for a nil document.
func (d ValidDocument) QuoteFinder() *QuoteFinder {
	return &QuoteFinder{doc: d.doc, valid: true}
}

// EditQuoteFinder is QuoteFinder for a document edit's quotes - a replace's or a delete's find,
// an insert's or a move's anchor - which no edit can take across two textblocks: a match that
// spans two or more counts toward neither the quote's matches nor its ambiguity (cascade), so a
// quote with one match inside one textblock resolves to it whatever spans it also has, and one
// with two or more there is ErrTargetAmbiguous naming those alone. A quote whose only matches span
// textblocks resolves to the first of them, which the edit refuses as spanning blocks. A comment's
// or an ask's anchor keeps QuoteFinder's count, spans included (dispatch://INFRA-4345, R1).
func (d ValidDocument) EditQuoteFinder() *QuoteFinder {
	return &QuoteFinder{doc: d.doc, valid: true, inOneTextblock: true}
}

// Find is FindQuote(doc, quote, occurrence, near): the same range or the same error.
func (f *QuoteFinder) Find(quote string, occurrence, near *int) (Range, error) {
	r, _, err := f.FindSpelling(quote, occurrence, near)
	return r, err
}

// FindSpelling is Find, and the inline markup the quote spells over the range it matched
// (Spelling): a document edit reads its find as markup only where the finder did.
func (f *QuoteFinder) FindSpelling(quote string, occurrence, near *int) (Range, Spelling, error) {
	matches, err := f.matches(quote)
	if err != nil {
		return Range{}, Spelling{}, err
	}
	match, err := f.choose(quote, matches, occurrence, near)
	if err != nil {
		return Range{}, Spelling{}, err
	}
	return match.Range, f.text.spelling(match), nil
}

// FindWritten is Find over the matchers that read a quote as written - exactly, then with its
// whitespace normalized (writtenCascade) - and not as markdown: a quote that opens with a list
// item's marker selects an item where it matches no text as written (FindListItem), and the
// markdown reading, which takes the marker for list syntax and drops it, comes after that
// selection. Only a document edit asks it; the range is Find's for a quote either matcher holds.
func (f *QuoteFinder) FindWritten(quote string, occurrence *int) (Range, error) {
	if err := f.prepare(); err != nil {
		return Range{}, err
	}
	matches := f.cascade(writtenCascade, quote)
	match, err := f.choose(quote, matches, occurrence, nil)
	if err != nil {
		return Range{}, err
	}
	return match.Range, nil
}

// FindListItem answers a quote that opens with a list item's marker (ParseListItemQuote) by the
// items it selects (listItemQuoteMatches), choosing among them as Find chooses among a quote's
// matches, and the markup the item's text spells over the range it matched, as FindSpelling
// reports it; a quote that selects no item is Find's miss. Only a document edit asks it, after
// Find matched nothing: a quote a comment or an ask anchors is text the document held, whose
// marker is text, so no anchor is ever read as a selector.
func (f *QuoteFinder) FindListItem(quote string, occurrence *int) (Range, Spelling, error) {
	if err := f.prepare(); err != nil {
		return Range{}, Spelling{}, err
	}
	item := ParseListItemQuote(quote)
	if item.Text == "" {
		return Range{}, Spelling{}, quoteNotFound(f.doc, quote)
	}
	match, err := f.choose(quote, listItemQuoteMatches(f.doc, f.text, item), occurrence, nil)
	if err != nil {
		return Range{}, Spelling{}, err
	}
	return match.Range, f.text.spelling(match), nil
}

// choose is the match quote's matches resolve to: the one the occurrence names, the only one, or
// the one nearest near; otherwise the miss or the ambiguity Find answers.
func (f *QuoteFinder) choose(quote string, matches []quoteMatch, occurrence, near *int) (quoteMatch, error) {
	doc, text := f.doc, f.text
	if len(matches) == 0 {
		return quoteMatch{}, quoteNotFound(doc, quote)
	}
	// An edit's finder answers matches inside one textblock alone, or, where the quote has none
	// there, the spans of its first matcher that had any (cascade). Those resolve to the first,
	// which the edit refuses as spanning blocks, and count toward neither an occurrence nor an
	// ambiguity.
	if f.inOneTextblock && !f.insideOneTextblock(matches[0].Range) {
		return matches[0], nil
	}
	if occurrence != nil {
		if *occurrence < 0 || *occurrence >= len(matches) {
			miss := quoteNotFound(doc, quote)
			var missing *ErrQuoteNotFound
			if errors.As(miss, &missing) {
				missing.Matches = len(matches)
			}
			return quoteMatch{}, miss
		}
		return matches[*occurrence], nil
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if near != nil {
		// MinFunc returns the first least match, which nearerFirst's earlier-of-two-as-near tie
		// makes the first NearestFirst lists.
		return slices.MinFunc(matches, nearerFirst(*near)), nil
	}

	candidates := make([]Candidate, 0, len(matches))
	for _, match := range matches {
		candidates = append(candidates, Candidate{
			Range:   match.Range,
			Context: context16(text.value, max(0, match.textFrom-40), min(len16(text.value), match.textTo+40)),
		})
	}
	return quoteMatch{}, &ErrTargetAmbiguous{Target: quote, Candidates: candidates}
}

// NearestFirst is every range quote matches, as Find matches it, ordered as Find with near chooses
// among them: the nearest to near first, and of two as near, the earlier. Find with near returns
// its first. A quote that matches nothing is ErrTargetNotFound itself, without ranking the
// document's blocks for the nearest ones the way Find's miss does, which costs another walk of the
// document: a caller that drops the miss pays nothing to describe it. A caller whose first choice
// cannot carry what it writes there takes the next.
func (f *QuoteFinder) NearestFirst(quote string, near int) ([]Range, error) {
	matches, err := f.matches(quote)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, ErrTargetNotFound
	}
	slices.SortFunc(matches, nearerFirst(near))
	ranges := make([]Range, len(matches))
	for index, match := range matches {
		ranges[index] = match.Range
	}
	return ranges, nil
}

// nearerFirst orders two matches of one quote as Find with near chooses between them: the nearer to
// near first, and of two as near, the earlier. NearestFirst sorts by it and Find takes its least,
// so the range Find chooses and the first NearestFirst lists are one rule, held in one place.
func nearerFirst(near int) func(left, right quoteMatch) int {
	return func(left, right quoteMatch) int {
		if byDistance := distance(left.From, near) - distance(right.From, near); byDistance != 0 {
			return byDistance
		}
		return left.From - right.From
	}
}

// matches is every match of quote in the finder's document: a heading quote's headings, otherwise
// the plain matcher cascade's.
func (f *QuoteFinder) matches(quote string) ([]quoteMatch, error) {
	if err := f.prepare(); err != nil {
		return nil, err
	}
	if marker := atxHeadingMarker.FindString(quote); marker != "" && marker != quote {
		return headingQuoteMatches(f.doc, f.text, quote[len(marker):]), nil
	}
	return f.cascade(quoteCascade[:], quote), nil
}

// cascade is the matches of quote's first matcher of matchers that has any, as quoteMatches takes
// them. An edit's finder (EditQuoteFinder) counts only a match inside one textblock: a matcher
// whose every match spans two or more gives way to the next, and where no matcher has a match
// inside one, the matches are the spans of the first that had any, which choose resolves to the
// first of. So what an edit's finder answers is matches all inside one textblock, or all spans. A
// heading quote's matches and a list item's selection lie inside one textblock by their own rule
// (headingQuoteMatches, listItemQuoteMatches), so they need no reading here.
func (f *QuoteFinder) cascade(matchers []func(flattenedText, string) []quoteMatch, quote string) []quoteMatch {
	var spans []quoteMatch
	for _, matcher := range matchers {
		matches := matcher(f.text, quote)
		if len(matches) == 0 {
			continue
		}
		if !f.inOneTextblock {
			return matches
		}
		if inside := f.withinTextblocks(matches); len(inside) > 0 {
			return inside
		}
		if spans == nil {
			spans = matches
		}
	}
	return spans
}

// withinTextblocks is matches without those that span two or more textblocks: matches itself
// where none does, so the common quote allocates nothing here.
func (f *QuoteFinder) withinTextblocks(matches []quoteMatch) []quoteMatch {
	for index, match := range matches {
		if f.insideOneTextblock(match.Range) {
			continue
		}
		inside := slices.Clone(matches[:index])
		for _, rest := range matches[index+1:] {
			if f.insideOneTextblock(rest.Range) {
				inside = append(inside, rest)
			}
		}
		return inside
	}
	return matches
}

// insideOneTextblock reports whether r lies wholly inside one textblock, TargetSpansBlocks' answer
// negated, read off the textblocks' content ranges rather than off a walk of the document for each
// match: the walk that collects them runs once, at the finder's first such question.
func (f *QuoteFinder) insideOneTextblock(r Range) bool {
	if !f.textblocksDone {
		f.textblocksDone = true
		walk(f.doc, func(node *Node, _ []int, pos, end int) bool {
			if isTextblock(node.Type) {
				f.textblocks = append(f.textblocks, Range{From: pos + 1, To: end - 1})
			}
			return true
		})
	}
	// The textblocks' contents are disjoint and in document order, since no textblock holds
	// another, so the only one that can hold r is the last to start at or before r.From.
	at := sort.Search(len(f.textblocks), func(index int) bool { return f.textblocks[index].From > r.From }) - 1
	return at >= 0 && r.To <= f.textblocks[at].To
}

// prepare validates the finder's document and flattens its text at the first call, and answers
// what that found every call after.
func (f *QuoteFinder) prepare() error {
	if f.doc == nil {
		return fmt.Errorf("%w: FindQuote wants a document", ErrSchema)
	}
	if !f.prepared {
		f.prepared = true
		if !f.valid {
			f.err = f.doc.Validate()
		}
		if f.err == nil {
			f.text = buildFlattenedText(f.doc, false)
		}
	}
	return f.err
}

// listItemTaskBox is a task item's box, `[ ]` or `[x]`, opening what follows a list marker: the
// box renders as the item's checkbox, not as text the item holds.
var listItemTaskBox = regexp.MustCompile(`^\[([ xX])\][ \t]+`)

// ListItemQuote is a quote that opens with a list item's marker read as what the marker selects:
// an item of a list of Kind whose text starts with Text, and, when a task box (`[ ]`, `[x]`)
// follows the marker, a task item checked or not as Checked says.
type ListItemQuote struct {
	Kind    MarkerKind
	Checked *bool
	Text    string
}

// ParseListItemQuote reads a quote that opens with a list item's marker (`- `, `* `, `+ `, `5. `,
// `5) `), and a task box after it, as the item they select. A quote opening with no marker, or
// holding nothing after it, selects no item: its Text is empty.
func ParseListItemQuote(quote string) ListItemQuote {
	marker, width := LeadingBlockMarker(quote)
	if marker.Kind != MarkerOrdered && marker.Kind != MarkerBullet {
		return ListItemQuote{}
	}
	item := ListItemQuote{Kind: marker.Kind, Text: quote[width:]}
	if box := listItemTaskBox.FindStringSubmatch(item.Text); box != nil {
		checked := box[1] != " "
		item.Checked = &checked
		item.Text = item.Text[len(box[0]):]
	}
	return item
}

// listItemQuoteMatches is every match of item's text that begins where an item it selects begins
// its text: the marker selects the item, as a heading quote's marker selects a heading, and the
// renderer writes it before the item's first paragraph, so the text it is quoted with starts
// there. The item's number is not matched, only its kind, and a task box only a task item whose
// box is in that state. Each step of the matcher cascade is kept to those starts before the next
// is tried, so an exact match elsewhere does not hide an item the text matches with its
// whitespace normalized.
func listItemQuoteMatches(doc *Node, text flattenedText, item ListItemQuote) []quoteMatch {
	type start struct{ from, end int }
	var starts []start
	walk(doc, func(node *Node, path []int, pos, end int) bool {
		if node.Type != "paragraph" || textblockMarker(doc, node, path).Kind != item.Kind {
			return true
		}
		if item.Checked != nil {
			// textblockMarker found a list item's first paragraph, so its parent is that item.
			checked, task := nodeAtPath(doc, path[:len(path)-1]).Attrs["checked"].(bool)
			if !task || checked != *item.Checked {
				return true
			}
		}
		starts = append(starts, start{from: pos + 1, end: end - 1})
		return true
	})
	if len(starts) == 0 {
		return nil
	}
	for _, matcher := range quoteCascade {
		var matches []quoteMatch
		for _, match := range matcher(text, item.Text) {
			for _, s := range starts {
				if match.From == s.from && match.To <= s.end {
					matches = append(matches, match)
					break
				}
			}
		}
		if len(matches) > 0 {
			return matches
		}
	}
	return nil
}

// TargetSpansBlocks reports whether r is not wholly contained by one
// textblock. Inline code and links remain inside their containing textblock.
func TargetSpansBlocks(doc *Node, r Range) bool {
	withinTextblock := false
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if isTextblock(node.Type) && pos+1 <= r.From && r.To <= end-1 {
			withinTextblock = true
			return false
		}
		return true
	})
	return !withinTextblock
}

// FindHeading finds the heading whose text, as a read serves it (TextContent), equals title
// exactly. occurrence is zero-based; without it, repeated titles return ErrTargetAmbiguous.
// It validates doc first: a nil tree wants a document, and one Validate refuses is Validate's
// error. It validates the tree rather than the document (ValidateDocument), so a tree that is no
// document is searched for its headings as it always was, and finds none.
func FindHeading(doc *Node, title string, occurrence *int) (Range, error) {
	if doc == nil {
		return Range{}, wantsDocument("FindHeading")
	}
	if err := doc.Validate(); err != nil {
		return Range{}, err
	}
	return findHeading(doc, title, occurrence)
}

// FindHeading is FindHeading over d's tree without validating it again; the zero ValidDocument
// wants a document, as FindHeading's nil tree does.
func (d ValidDocument) FindHeading(title string, occurrence *int) (Range, error) {
	if d.doc == nil {
		return Range{}, wantsDocument("FindHeading")
	}
	return findHeading(d.doc, title, occurrence)
}

// findHeading is the heading search itself, over a tree its caller has validated.
func findHeading(doc *Node, title string, occurrence *int) (Range, error) {
	var matches []Candidate
	pos := 0
	for _, child := range doc.Children {
		end := pos + nodeSize(child)
		if child.Type == "heading" {
			headingText := TextContent(child)
			if headingText == title {
				matches = append(matches, Candidate{
					Range:   Range{From: pos, To: end},
					Context: headingText,
				})
			}
		}
		pos = end
	}
	if len(matches) == 0 {
		return Range{}, headingNotFound(doc, title)
	}
	if occurrence != nil {
		if *occurrence < 0 || *occurrence >= len(matches) {
			return Range{}, headingNotFound(doc, title)
		}
		return matches[*occurrence].Range, nil
	}
	if len(matches) == 1 {
		return matches[0].Range, nil
	}
	return Range{}, &ErrTargetAmbiguous{Target: title, Candidates: matches}
}

// headingNotFound ranks the document's headings by their common prefix with title, the way a
// quote miss ranks blocks: the anchor that missed is usually a prefix of the one meant.
func headingNotFound(doc *Node, title string) error {
	type ranked struct {
		text   string
		prefix int
	}
	var candidates []ranked
	for _, child := range doc.Children {
		if child.Type != "heading" {
			continue
		}
		heading := TextContent(child)
		candidates = append(candidates, ranked{text: heading, prefix: commonPrefixLength(title, heading)})
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		return candidates[left].prefix > candidates[right].prefix
	})
	nearest := make([]string, 0, nearestBlockCount)
	for _, candidate := range candidates[:min(nearestBlockCount, len(candidates))] {
		nearest = append(nearest, firstRunes(candidate.text, 120))
	}
	return &ErrHeadingNotFound{Title: title, Nearest: nearest}
}

// Size returns the ProseMirror position immediately after the last block.
func Size(doc *Node) int {
	return nodeSize(doc)
}

type inlineMarkup uint8

const (
	inlineMarkupCode inlineMarkup = 1 << iota
	inlineMarkupStrong
	inlineMarkupEmphasis
	inlineMarkupLink
)

// flattenedText is a document's inline text in one string, with each UTF-16 unit's
// ProseMirror position and inline markup. forms caches the whole-text derived views the
// quote matchers read, so one FindQuote computes each at most once however far it falls
// through the exact -> normalized -> markdown chain, and a QuoteFinder at most once for all
// the quotes it answers.
type flattenedText struct {
	value     string
	positions []int
	marks     []inlineMarkup
	// links is the link each run of linked units is under, in unit order, for a quote read as
	// markdown alone (renderedMarkdownQuote): what a quote spells a link with is its markup, its
	// URL and its title (Spelling), and a document's link carries its own on its mark.
	links []linkedUnits
	forms *textForms
}

// linkedUnits is a run of a flattened text's units, from from up to to, under link.
type linkedUnits struct {
	from, to int
	link     linkTarget
}

// linkTarget is what a link mark carries beside its text: the URL it links to and its title, ""
// for none. The renderer writes both (`[this](https://a.example/1 "Spec")`), so both are the
// markup a find spells the link with.
type linkTarget struct {
	href, title string
}

// markLinkTarget is the URL and the title the link mark carries.
func markLinkTarget(mark Mark) linkTarget {
	href, _ := mark.Attrs["href"].(string)
	title, _ := mark.Attrs["title"].(string)
	return linkTarget{href: href, title: title}
}

// linkIn is the URL and title of the first link over a unit of span, and none where no link
// covers one.
func (t flattenedText) linkIn(span range16) linkTarget {
	index := sort.Search(len(t.links), func(index int) bool { return t.links[index].to > span.from })
	if index < len(t.links) && t.links[index].from < span.to {
		return t.links[index].link
	}
	return linkTarget{}
}

type textForms struct {
	units          []uint16
	unitsDone      bool
	normalized     []rune
	spans          []range16
	normalizedDone bool
}

// units16 is value encoded as UTF-16 units, computed on first use.
func (t flattenedText) units16() []uint16 {
	if !t.forms.unitsDone {
		t.forms.units = utf16.Encode([]rune(t.value))
		t.forms.unitsDone = true
	}
	return t.forms.units
}

// normalizedForm is normalizedRanges(value), computed on first use. The matchers compare runes,
// so the normalized text is kept as runes: a finder answering many quotes converts it once.
func (t flattenedText) normalizedForm() ([]rune, []range16) {
	if !t.forms.normalizedDone {
		t.forms.normalized, t.forms.spans = normalizedRanges(t.value)
		t.forms.normalizedDone = true
	}
	return t.forms.normalized, t.forms.spans
}

// flattenedTextSize is how much buildFlattenedText's second pass appends over doc: units is how
// many UTF-16 units it writes, which is one positions entry and one marks entry each, and bytes is
// how many bytes the builder holds. It walks the document once more (Walk), measuring no node and
// allocating nothing, which costs less than the doubling the appends pay for without it.
//
// Per text node the count is exact. nulAsReplacement writes each U+0000 as U+FFFD, three bytes in
// place of one but one UTF-16 unit either way, so only the byte count moves for it; len16 counts
// the units of the text as it stands, replaced or not.
//
// The gaps are counted high: the second pass writes a space, one position and one mark only
// between two text nodes whose ProseMirror ranges do not touch, and this counts one gap for every
// text node after the first.
//
// The count sets capacity alone (Grow, make with a capacity), so it decides how often the build
// grows a buffer and nothing about what the build writes: a count that comes out low costs a
// regrowth, and one that comes out high leaves capacity unused. Low is possible for a text holding
// invalid UTF-8, where len counts a stray byte once and the build writes the U+FFFD that ranging
// over it yields, three bytes.
func flattenedTextSize(doc *Node) (units, bytes int) {
	texts := 0
	Walk(doc, func(node *Node) bool {
		if node.Type != "text" {
			return true
		}
		texts++
		units += len16(node.Text)
		bytes += len(node.Text) + 2*strings.Count(node.Text, "\x00")
		return true
	})
	if texts > 0 {
		units += texts - 1
		bytes += texts - 1
	}
	return units, bytes
}

// buildFlattenedText reads doc's inline text into one string with the index the matchers read it
// through. The string and the index are allocated once, at the size the document measures
// (flattenedTextSize), rather than grown: growing the index copied the whole of it at every
// doubling, which on a document whose paragraph is a mebibyte long was most of what a cold
// one-word edit allocated (sjawhar/legion#2522). links says to record the URL and title of each
// link's units (flattenedText.links), which only a quote's reading needs.
func buildFlattenedText(doc *Node, links bool) flattenedText {
	units, bytes := flattenedTextSize(doc)
	var out strings.Builder
	out.Grow(bytes)
	positions := make([]int, 0, units)
	marks := make([]inlineMarkup, 0, units)
	var linked []linkedUnits
	lastEnd := -1
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if node.Type != "text" {
			return true
		}
		if lastEnd >= 0 && lastEnd != pos {
			out.WriteByte(' ')
			positions = append(positions, lastEnd)
			marks = append(marks, 0)
		}
		markup := nodeInlineMarkup(node)
		from := len(positions)
		// A quote is matched against the text every read serves, so a browser's U+0000 is U+FFFD
		// here too; both are one UTF-16 unit, so no position moves.
		for _, char := range nulAsReplacement(node.Text) {
			out.WriteRune(char)
			width := 1
			if char > 0xffff {
				width = 2
			}
			for offset := range width {
				positions = append(positions, pos+offset)
				marks = append(marks, markup)
			}
			pos += width
		}
		if links && markup&inlineMarkupLink != 0 {
			linked = append(linked, linkedUnits{from: from, to: len(positions), link: linkOf(node.Marks)})
		}
		lastEnd = end
		return true
	})
	return flattenedText{value: out.String(), positions: positions, marks: marks, links: linked, forms: &textForms{}}
}

// linkOf is the URL and the title of the link among marks, and none where none is a link.
func linkOf(marks []Mark) linkTarget {
	for _, mark := range marks {
		if mark.Type == "link" {
			return markLinkTarget(mark)
		}
	}
	return linkTarget{}
}

func nodeInlineMarkup(node *Node) inlineMarkup {
	var markup inlineMarkup
	for _, mark := range node.Marks {
		markup |= markTypeMarkup(mark.Type)
	}
	return markup
}

// markTypeMarkup is the inline markup a mark of markType is read as by the markdown matcher, or 0
// for a mark it does not read: strikethrough, and every anchor.
func markTypeMarkup(markType string) inlineMarkup {
	switch markType {
	case "inlineCode":
		return inlineMarkupCode
	case "strong":
		return inlineMarkupStrong
	case "emphasis":
		return inlineMarkupEmphasis
	case "link":
		return inlineMarkupLink
	}
	return 0
}

type quoteMatch struct {
	Range
	textFrom int
	textTo   int
	// quote is the quote read as markdown, for a match the markdown matcher made, and offset the
	// rune of the text's normalized form the match starts at, where the matcher paired the
	// quote's runes with the text's (normalizedMarkdownQuoteMatches); quote is nil for a match
	// of the text as written.
	quote  *flattenedText
	offset int
}

// Spelling is the inline markup a quote spells over the range it matched, as the finder read it:
// for a quote matched as markdown (markdownQuoteMatches), the markup the quote writes over each
// UTF-16 unit of the match's text, as the matcher paired the quote's text with the document's,
// and the URL and title of the link it writes there. A quote matched as written, exactly or with
// its whitespace normalized, spells none: a `*`, `_` or backtick in it is text the document holds,
// not markup. The zero Spelling spells nothing.
type Spelling struct {
	units []spelledUnit
}

// spelledUnit is one UTF-16 unit of a match's text: its ProseMirror position, the inline markup
// the quote writes over it, and the URL and title of the link the quote writes over it, none for
// no link.
type spelledUnit struct {
	pos    int
	markup inlineMarkup
	link   linkTarget
}

// spelling is how match's quote spells the text of t it matched: none for a match of the text as
// written, and otherwise the markup of each quote rune over each unit of the text rune the
// matcher paired it with, with the URL and title of the quote's link over that rune.
func (t flattenedText) spelling(match quoteMatch) Spelling {
	if match.quote == nil {
		return Spelling{}
	}
	_, textSpans := t.normalizedForm()
	_, quoteSpans := match.quote.normalizedForm()
	units := make([]spelledUnit, 0, match.textTo-match.textFrom)
	for index, quoteSpan := range quoteSpans {
		markup := markupInRange(match.quote.marks, quoteSpan)
		var link linkTarget
		if markup&inlineMarkupLink != 0 {
			link = match.quote.linkIn(quoteSpan)
		}
		textSpan := textSpans[match.offset+index]
		for unit := textSpan.from; unit < textSpan.to; unit++ {
			units = append(units, spelledUnit{pos: t.positions[unit], markup: markup, link: link})
		}
	}
	return Spelling{units: units}
}

// spells reports whether s writes mark's markup over every unit of its match at a position from
// from to to, and over one at least: the quote spells that markup over that stretch of the match,
// not over another stretch of it. A link is spelled only with its own URL and its own title: a
// find that writes the link's text under another URL, or with another title or none, copied from
// an older read after another writer changed the link, spells another link (spellsAnotherLink),
// and what the document holds would go with no word. A mark the markdown matcher does not read
// (markTypeMarkup), strikethrough or an anchor, is never spelled.
func (s Spelling) spells(mark Mark, from, to int) bool {
	markup, own := s.writes(mark, from, to)
	return markup && own
}

// spellsAnotherLink reports whether s writes the markup of mark, a link, over every unit of its
// match at a position from from to to, and over one at least, and under a URL or a title other
// than the link's own over one of them: the quote spells another link there. A mark that is no
// link is never spelled as another.
func (s Spelling) spellsAnotherLink(mark Mark, from, to int) bool {
	markup, own := s.writes(mark, from, to)
	return markup && !own
}

// writes reports whether s writes mark's markup over every unit of its match at a position from
// from to to, and over one at least (markup), and whether it writes each of those units under
// mark's own URL and title (own), as it does every unit for a mark that is no link.
func (s Spelling) writes(mark Mark, from, to int) (markup, own bool) {
	kind := markTypeMarkup(mark.Type)
	if kind == 0 {
		return false, false
	}
	link := kind == inlineMarkupLink
	var target linkTarget
	if link {
		target = markLinkTarget(mark)
	}
	own = true
	for _, unit := range s.units {
		if unit.pos < from || unit.pos >= to {
			continue
		}
		if unit.markup&kind == 0 {
			return false, false
		}
		if link && unit.link != target {
			own = false
		}
		markup = true
	}
	return markup, own
}

// quoteCascade is the matcher cascade a plain quote runs through, in its order: exact text first,
// then whitespace-normalized text, then the quote read as markdown against inline markup. A quote
// takes the matches of the first matcher that has any (quoteMatches), and a list item's quote the
// first that has any at a start of an item it selects (listItemQuoteMatches). writtenCascade is
// its first two, which read the quote as written (FindWritten).
var (
	quoteCascade   = [...]func(flattenedText, string) []quoteMatch{exactQuoteMatches, normalizedQuoteMatches, markdownQuoteMatches}
	writtenCascade = quoteCascade[:2]
)

// quoteMatches is the matches of quote's first matcher in the cascade (quoteCascade) that has
// any, and none where no matcher has any.
func quoteMatches(text flattenedText, quote string) []quoteMatch {
	for _, matcher := range quoteCascade {
		if matches := matcher(text, quote); len(matches) > 0 {
			return matches
		}
	}
	return nil
}

func exactQuoteMatches(text flattenedText, quote string) []quoteMatch {
	needle := utf16.Encode([]rune(quote))
	var matches []quoteMatch
	for _, offset := range findAllUnits(text.units16(), needle) {
		matches = append(matches, quoteMatch{
			Range: Range{
				From: text.positions[offset],
				To:   text.positions[offset+len(needle)-1] + 1,
			},
			textFrom: offset,
			textTo:   offset + len(needle),
		})
	}
	return matches
}

type range16 struct {
	from int
	to   int
}

func normalizedQuoteMatches(text flattenedText, quote string) []quoteMatch {
	normalizedText, textSpans := text.normalizedForm()
	needle, quoteSpans := normalizedRanges(quote)
	if len(needle) == 0 || len(textSpans) == 0 || len(quoteSpans) == 0 {
		return nil
	}

	var matches []quoteMatch
	for _, offset := range findAllRunes(normalizedText, needle) {
		from := textSpans[offset].from
		to := textSpans[offset+len(needle)-1].to
		matches = append(matches, quoteMatch{
			Range: Range{
				From: text.positions[from],
				To:   text.positions[to-1] + 1,
			},
			textFrom: from,
			textTo:   to,
		})
	}
	return matches
}

func markdownQuoteMatches(text flattenedText, quote string) []quoteMatch {
	rendered, marked := renderedMarkdownQuote(quote)
	if !marked {
		return nil
	}
	return normalizedMarkdownQuoteMatches(text, &rendered)
}

// renderedMarkdownQuote is quote read as markdown, when its text and inline markup differ from
// quote's own. A quote matches by its text, which a short table row's padded cells do not hold, so
// its tables are read only while they need no padding (quoteBudget), and a quote each
// operation of an edit batch resolves costs no padded cell. A quote that makes more elements than
// a write may is refused before goldmark builds the rest of it, and reads as no markdown.
func renderedMarkdownQuote(quote string) (flattenedText, bool) {
	parsed, err := parseStamped(quote, quoteBudget())
	if err != nil {
		return flattenedText{}, false
	}
	rendered := buildFlattenedText(parsed, true)
	if rendered.value == quote || !hasInlineMarkup(rendered.marks) {
		return flattenedText{}, false
	}
	return rendered, true
}

func hasInlineMarkup(marks []inlineMarkup) bool {
	for _, markup := range marks {
		if markup != 0 {
			return true
		}
	}
	return false
}

// normalizedMarkdownQuoteMatches is every match of quote, read as markdown, in text with the
// whitespace of both normalized, where text carries every inline markup the quote writes over
// each rune the matcher pairs with the quote's. Each match keeps quote and the rune it starts at,
// so the markup the quote spells over each part of it can be read back (flattenedText.spelling).
func normalizedMarkdownQuoteMatches(text flattenedText, quote *flattenedText) []quoteMatch {
	normalizedText, textSpans := text.normalizedForm()
	needle, quoteSpans := quote.normalizedForm()
	if len(needle) == 0 || len(textSpans) == 0 || len(quoteSpans) == 0 {
		return nil
	}

	var matches []quoteMatch
	for _, offset := range findAllRunes(normalizedText, needle) {
		if !matchingInlineMarkup(text, *quote, textSpans[offset:offset+len(needle)], quoteSpans) {
			continue
		}
		from := textSpans[offset].from
		to := textSpans[offset+len(needle)-1].to
		matches = append(matches, quoteMatch{
			Range: Range{
				From: text.positions[from],
				To:   text.positions[to-1] + 1,
			},
			textFrom: from,
			textTo:   to,
			quote:    quote,
			offset:   offset,
		})
	}
	return matches
}

func matchingInlineMarkup(text, quote flattenedText, textSpans, quoteSpans []range16) bool {
	for index, quoteSpan := range quoteSpans {
		required := markupInRange(quote.marks, quoteSpan)
		if required == 0 {
			continue
		}
		if markupInRange(text.marks, textSpans[index])&required != required {
			return false
		}
	}
	return true
}

func markupInRange(marks []inlineMarkup, span range16) inlineMarkup {
	var markup inlineMarkup
	for index := span.from; index < span.to; index++ {
		markup |= marks[index]
	}
	return markup
}

func quoteNotFound(doc *Node, quote string) error {
	if marker := atxHeadingMarker.FindString(quote); marker != "" {
		quote = quote[len(marker):]
	} else if item := ParseListItemQuote(quote); item.Text != "" {
		quote = item.Text
	} else if rendered, marked := renderedMarkdownQuote(quote); marked {
		quote = rendered.value
	}
	return &ErrQuoteNotFound{Nearest: nearestBlocks(doc, quote, nearestBlockCount)}
}

// nearestBlocks ranks every textblock by the length of its common prefix with quote and
// returns the top `limit` (ties keep document order), each trimmed to 80 runes.
func nearestBlocks(doc *Node, quote string, limit int) []string {
	type ranked struct {
		text   string
		prefix int
	}
	var candidates []ranked
	walk(doc, func(node *Node, _ []int, _, _ int) bool {
		if isTextblock(node.Type) {
			candidate := TextContent(node)
			candidates = append(candidates, ranked{text: candidate, prefix: commonPrefixLength(quote, candidate)})
		}
		return true
	})
	sort.SliceStable(candidates, func(left, right int) bool {
		return candidates[left].prefix > candidates[right].prefix
	})
	nearest := make([]string, 0, limit)
	for _, candidate := range candidates[:min(limit, len(candidates))] {
		nearest = append(nearest, firstRunes(candidate.text, 80))
	}
	return nearest
}

// headingQuoteMatches returns the text range of every heading whose rendered text equals
// title, so a quote written with its ATX marker (`## Title`) selects headings only, each as the
// matcher cascade matched title.
func headingQuoteMatches(doc *Node, text flattenedText, title string) []quoteMatch {
	if title == "" {
		return nil
	}
	candidates := quoteMatches(text, title)
	var matches []quoteMatch
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if node.Type != "heading" {
			return true
		}
		for _, match := range candidates {
			if pos+1 <= match.From && match.To <= end-1 {
				matches = append(matches, match)
			}
		}
		return true
	})
	return matches
}

// TextContent is node's text, hard breaks as line feeds, as a read serves it: each U+0000 a browser
// edit left as U+FFFD (nulAsReplacement). An ask row's question and options, a heading anchor and
// a miss's nearest blocks and headings are read through it.
func TextContent(node *Node) string {
	if node.Type == "text" {
		return nulAsReplacement(node.Text)
	}
	if node.Type == "hardbreak" {
		return "\n"
	}
	var out strings.Builder
	for _, child := range node.Children {
		out.WriteString(TextContent(child))
	}
	return out.String()
}

func commonPrefixLength(left, right string) int {
	prefix := 0
	rightOffset := 0
	for _, leftRune := range left {
		if rightOffset == len(right) {
			return prefix
		}
		rightRune, width := utf8.DecodeRuneInString(right[rightOffset:])
		if leftRune != rightRune {
			return prefix
		}
		rightOffset += width
		prefix++
	}
	return prefix
}

func firstRunes(value string, count int) string {
	for index := range value {
		if count == 0 {
			return value[:index]
		}
		count--
	}
	return value
}

// findAllUnits is every position in haystack at which the UTF-16 units of needle begin, in
// ascending order, and nil where there is none.
func findAllUnits(haystack, needle []uint16) []int {
	return findAll(haystack, needle)
}

// findAllRunes is every position in haystack at which the runes of needle begin, in ascending
// order, and nil where there is none.
func findAllRunes(haystack, needle []rune) []int {
	return findAll(haystack, needle)
}

// findAll is every position in haystack at which needle begins, in ascending order, overlapping
// matches included, and nil where needle is empty, longer than haystack, or found nowhere.
//
// It is Knuth-Morris-Pratt: one pass over needle builds its borders, where borders[index] is the
// length of the longest proper prefix of needle[:index+1] that is also its suffix, and one pass
// over haystack reads each element once, falling back along those borders where an element
// differs instead of comparing the needle again from the next start. The scan it replaced
// compared the whole needle at every start, so a quote that almost matches everywhere cost the
// length of the text times the length of the quote - half a second for a mebibyte of text and a
// kibibyte of quote - where this costs the length of the text plus the length of the quote
// whatever the two hold.
//
// The package searches two element types, a text's UTF-16 units and its normalized form's runes,
// and the algorithm reads its elements only through ==, so it is written once over the element
// type rather than twice over the same shape; findAllUnits and findAllRunes keep the names the
// matchers call.
func findAll[E comparable](haystack, needle []E) []int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return nil
	}
	borders := make([]int, len(needle))
	for index, length := 1, 0; index < len(needle); index++ {
		for length > 0 && needle[index] != needle[length] {
			length = borders[length-1]
		}
		if needle[index] == needle[length] {
			length++
		}
		borders[index] = length
	}
	var matches []int
	matched := 0
	for index, element := range haystack {
		for matched > 0 && element != needle[matched] {
			matched = borders[matched-1]
		}
		if element == needle[matched] {
			matched++
		}
		if matched == len(needle) {
			matches = append(matches, index-len(needle)+1)
			matched = borders[matched-1]
		}
	}
	return matches
}

// normalizedRanges is value with each whitespace run read as one space and leading and trailing
// whitespace dropped, as runes, with the UTF-16 span of value each rune stands for.
func normalizedRanges(value string) ([]rune, []range16) {
	out := make([]rune, 0, len(value))
	spans := make([]range16, 0, len(value))
	unitOffset := 0
	inWhitespace := false
	whitespace := range16{}
	for _, value := range []rune(value) {
		width := len16(string(value))
		if unicode.IsSpace(value) {
			if !inWhitespace {
				whitespace = range16{from: unitOffset, to: unitOffset + width}
				inWhitespace = true
			} else {
				whitespace.to = unitOffset + width
			}
			unitOffset += width
			continue
		}
		if inWhitespace && len(out) > 0 {
			out = append(out, ' ')
			spans = append(spans, whitespace)
		}
		inWhitespace = false
		out = append(out, value)
		spans = append(spans, range16{from: unitOffset, to: unitOffset + width})
		unitOffset += width
	}
	return out, spans
}

func distance(left, right int) int {
	if left < right {
		return right - left
	}
	return left - right
}

// MarkSpans is the text a mark identity covers as ranges, in document order: its text runs, each
// joined to the next when no text without the mark lies between them, so a run at a textblock's
// end and one at the next textblock's start are one span across the boundary between them, as the
// browser editor takes them, while text without the mark ends a span.
func MarkSpans(doc *Node, markType, id string) []Range {
	var spans []Range
	between := false
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if node.Type != "text" {
			return true
		}
		if !nodeHasMarkID(node, markType, id) {
			between = true
			return true
		}
		if last := len(spans) - 1; last >= 0 && !between {
			spans[last].To = end
		} else {
			spans = append(spans, Range{From: pos, To: end})
		}
		between = false
		return true
	})
	return spans
}

// FindMark finds the first document-contiguous range covered by a mark identity, the first of
// its MarkSpans, and its text, the runs joined with a space where a block boundary lies between,
// each U+0000 written as U+FFFD (nulAsReplacement): the text is what an anchor row stores as its
// quote. It is FindMarks asked for the one identity, so one walk holds the rule for both.
func FindMark(doc *Node, markType, id string) (Range, string, bool) {
	key := MarkRef{Type: markType, ID: id}
	f, ok := FindMarks(doc, []MarkRef{key})[key]
	return f.Range, f.Quote, ok
}

// MarkFound is what FindMark and MarkAttrs read for one mark identity. Attrs is the attributes
// map of the identity's first mark in the tree itself, not a copy: callers read it and must not
// modify it.
type MarkFound struct {
	Range Range
	Quote string
	Attrs Attrs
}

// FindMarks is FindMark and MarkAttrs for many mark identities in one walk: each key it finds maps
// to the range and quote FindMark returns and the attributes MarkAttrs reads, uncopied, and a key it
// does not find is absent. A write refreshes every open anchor's range and quote, and a walk per
// anchor would make that refresh quadratic in a document carrying many of them; here a run costs
// only its own marks and the identities open at it.
func FindMarks(doc *Node, keys []MarkRef) map[MarkRef]MarkFound {
	if len(keys) == 0 {
		return map[MarkRef]MarkFound{}
	}
	wanted := make(map[MarkRef]*markSearch, len(keys))
	for _, key := range keys {
		if _, ok := wanted[key]; !ok {
			wanted[key] = &markSearch{last: -1}
		}
	}
	unfinished := len(wanted)
	var open []*markSearch
	run := 0
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		// Non-text nodes neither carry a mark nor end a span (MarkSpans).
		if node.Type != "text" {
			return true
		}
		run++
		for _, mark := range node.Marks {
			id, ok := mark.Attrs["id"].(string)
			if !ok {
				continue
			}
			search := wanted[MarkRef{Type: mark.Type, ID: id}]
			// A run carrying one identity twice counts once, and MarkAttrs reads the first of them.
			if search == nil || search.run == run || search.state == markClosed {
				continue
			}
			search.run = run
			if search.state == markUnseen {
				search.state = markOpen
				search.found = MarkFound{Range: Range{From: pos, To: pos}, Attrs: mark.Attrs}
				open = append(open, search)
			}
			search.add(node.Text, pos, end)
		}
		// A text run without an open identity ends its first span, and only the first counts.
		kept := open[:0]
		for _, search := range open {
			if search.run == run {
				kept = append(kept, search)
				continue
			}
			search.close()
			unfinished--
		}
		clear(open[len(kept):])
		open = kept
		return unfinished > 0
	})
	for _, search := range open {
		search.close()
	}
	found := make(map[MarkRef]MarkFound, len(wanted))
	for key, search := range wanted {
		if search.state != markUnseen {
			found[key] = search.found
		}
	}
	return found
}

type markSearchState uint8

const (
	markUnseen markSearchState = iota
	markOpen
	markClosed
)

// markSearch is one identity's progress through FindMarks' walk: unseen, then open over its first
// span, then closed for good.
type markSearch struct {
	state markSearchState
	found MarkFound
	// run is the text run that last carried the identity, so a run's sweep tells the identities it
	// carries from the open ones it ends.
	run   int
	quote strings.Builder
	// last is the end of the last run written to quote, or -1 before the first: a run starting
	// anywhere else is parted from it by a space.
	last int
	// pending holds the positions of empty runs in the span that are not yet written. The quote
	// takes a run only when its position lies before the span's end, so an empty run belongs to the
	// quote, writing a separator where it lies apart from the run before it, only once a later run
	// carries the span past it.
	pending []int
}

// add extends the open span over the run at pos..end and writes to the quote what the quote takes
// for it and for the empty runs that the span now passes.
func (s *markSearch) add(text string, pos, end int) {
	s.found.Range.To = end
	flushed := 0
	for flushed < len(s.pending) && s.pending[flushed] < end {
		s.write("", s.pending[flushed], s.pending[flushed])
		flushed++
	}
	s.pending = slices.Delete(s.pending, 0, flushed)
	if pos == end {
		s.pending = append(s.pending, pos)
		return
	}
	s.write(text, pos, end)
}

func (s *markSearch) write(text string, pos, end int) {
	if s.last >= 0 && pos != s.last {
		s.quote.WriteByte(' ')
	}
	s.quote.WriteString(text)
	s.last = end
}

// close ends the span: empty runs still pending lie at or past its end, which the quote never
// reaches.
func (s *markSearch) close() {
	s.state = markClosed
	s.found.Quote = nulAsReplacement(s.quote.String())
	s.quote = strings.Builder{}
	s.pending = nil
}

// WithoutQuotedMarkup is marks, each a span covering the whole of a match (MarksCovering), less
// each mark whose markup spelled, the quote's spelling of that match (QuoteFinder.FindSpelling),
// writes over the whole of it. A quote that spells a span's markup over the text it covers
// (`**Sami bold**`) leaves that mark to the text written over the match, which keeps it by writing
// it too or drops it by leaving it off; one that spells it over part of that text alone (`a **b**
// c` over `**a b c**`) spells that part's markup, not the span's, so the span keeps it. A quote
// matched as written spells nothing: its `*`, `_` or backtick is text the document holds (`2*3*4`
// in `*a 2\*3\*4 b*`), and one that writes a link's text under another URL or another title spells
// another link, so the link keeps its own. Strikethrough is never markup the finder reads, so it
// is never taken off.
func WithoutQuotedMarkup(marks []Mark, spelled Spelling) []Mark {
	if len(spelled.units) == 0 {
		return marks
	}
	return slices.DeleteFunc(marks, func(mark Mark) bool {
		return spelled.spells(mark, math.MinInt, math.MaxInt)
	})
}

type MarkRef struct {
	Type string
	ID   string
}

// Marked is a mark identity in the tree with the `by` attribute its first run carries, empty
// when it has none.
type Marked struct {
	MarkRef
	By string
}

// ListMarks returns each distinct Proof or Dispatch mark identity in document
// order, with the author its first run names. doc must be a tree Validate accepts: the walk
// descends into every node's children and computes no positions.
func ListMarks(doc *Node) []Marked {
	seen := make(map[MarkRef]struct{})
	var marked []Marked
	Walk(doc, func(node *Node) bool {
		if node.Type != "text" {
			return true
		}
		for _, mark := range node.Marks {
			if !strings.HasPrefix(mark.Type, "proof") && mark.Type != "dispatchAsk" {
				continue
			}
			id, ok := mark.Attrs["id"].(string)
			if !ok {
				continue
			}
			ref := MarkRef{Type: mark.Type, ID: id}
			if _, ok := seen[ref]; ok {
				continue
			}
			seen[ref] = struct{}{}
			by, _ := mark.Attrs["by"].(string)
			marked = append(marked, Marked{MarkRef: ref, By: by})
		}
		return true
	})
	return marked
}

// MarkAttrs returns a copy of the attributes on the first matching mark run: FindMarks' Attrs,
// copied so the caller may change them without changing the tree.
func MarkAttrs(doc *Node, markType, id string) (Attrs, bool) {
	key := MarkRef{Type: markType, ID: id}
	f, ok := FindMarks(doc, []MarkRef{key})[key]
	return cloneAttrs(f.Attrs), ok
}

// nodeHasMarkID reports whether node carries a markType mark whose id is id. A record mark's type
// does not exclude itself (anchorMarkTypes), so a run may carry two of one type, and the mark asked
// for need not be the first of them.
func nodeHasMarkID(node *Node, markType, id string) bool {
	for _, mark := range node.Marks {
		if markHasID(mark, markType, id) {
			return true
		}
	}
	return false
}

func markHasID(mark Mark, markType, id string) bool {
	if mark.Type != markType {
		return false
	}
	markID, ok := mark.Attrs["id"].(string)
	return ok && markID == id
}

// MarkRange adds mark to every selected text run. A mark may cross textblocks
// when the target schema allows it in each textblock.
func MarkRange(txn *crdt.Transaction, frag *crdt.YXmlFragment, r Range, mark Mark) error {
	return NewMarkWriter(frag).Mark(txn, r, mark)
}

// MarkWriter is MarkRange for many marks written to one fragment. It reads the fragment's text
// ranges once, at the first mark it writes, and reuses them for every later one: writing a mark
// formats Yjs texts, which changes neither a text's length nor the elements holding it, so the
// ranges stay those of the fragment. Nothing but its own marks may change the fragment while the
// writer is in use.
type MarkWriter struct {
	frag     *crdt.YXmlFragment
	prepared bool
	spans    []yTextRange
	err      error
}

// NewMarkWriter returns a writer over frag. It reads nothing of frag until its first mark.
func NewMarkWriter(frag *crdt.YXmlFragment) *MarkWriter {
	return &MarkWriter{frag: frag}
}

// Mark is MarkRange(txn, frag, r, mark): the same checks in the same order, the same Format calls
// and the same error. A fragment whose text ranges are refused refuses every mark.
func (w *MarkWriter) Mark(txn *crdt.Transaction, r Range, mark Mark) error {
	if txn == nil || w.frag == nil || r.From >= r.To {
		return fmt.Errorf("%w: invalid mark range", ErrSchema)
	}
	if !active.marks[mark.Type] {
		return fmt.Errorf("%w: mark %q", ErrSchema, mark.Type)
	}
	if err := attrNestingError("mark", mark.Type, mark.Attrs, maxMarkAttrNesting); err != nil {
		return err
	}

	if !w.prepared {
		w.prepared = true
		w.spans, w.err = yTextRanges(w.frag)
	}
	if w.err != nil {
		return w.err
	}
	// The ranges are in document order and disjoint, so the texts r overlaps are one run of them,
	// starting at the first that ends past r.From.
	spans := w.spans
	first := sort.Search(len(spans), func(index int) bool { return spans[index].To > r.From })
	last := first
	for ; last < len(spans) && spans[last].From < r.To; last++ {
		if !markAllowedInTextblock(mark.Type, spans[last].textblock) {
			return &MarkNotAllowedError{Mark: mark.Type, Textblock: spans[last].textblock, At: spans[last].From}
		}
	}
	if first == last {
		return ErrTargetNotFound
	}

	attributes := crdt.Attributes{markAttributeKey(mark): markAttributeValue(mark)}
	for _, span := range spans[first:last] {
		from := max(r.From, span.From) - span.From
		to := min(r.To, span.To) - span.From
		span.text.Format(txn, from, to-from, attributes)
	}
	return nil
}

func markAllowedInTextblock(markType, textblock string) bool {
	if textblock != "code_block" {
		return textblock == "paragraph" || textblock == "heading"
	}
	switch markType {
	case "proofAuthored", "proofSuggestion", "proofComment", "proofFlagged", "proofApproved":
		return true
	default:
		return false
	}
}

func markAttributeValue(mark Mark) crdt.Attributes {
	value := make(crdt.Attributes, len(mark.Attrs))
	for key, attr := range mark.Attrs {
		value[key] = attr
	}
	return value
}

type yTextRange struct {
	text      *crdt.YXmlText
	textblock string
	Range
}

type yTextNode struct {
	text      *crdt.YXmlText
	textblock string
}

type textLocation struct {
	pos int
	end int
}

// yTextRanges is the ProseMirror range of each non-empty Yjs text in frag, with the textblock
// holding it, in document order. MarkRange and Unmark call it on every anchored write, so it reads
// the fragment once and finds each text run's position as it goes.
func yTextRanges(frag *crdt.YXmlFragment) ([]yTextRange, error) {
	var texts []yTextNode
	var locations []textLocation
	if _, err := yFragmentLocations(frag, "", 1, 0, &texts, &locations); err != nil {
		return nil, err
	}

	ranges := make([]yTextRange, 0, len(texts))
	location := 0
	for _, current := range texts {
		length := current.text.Len()
		if length == 0 {
			continue
		}
		if location == len(locations) {
			return nil, fmt.Errorf("%w: Yjs text has no ProseMirror text node", ErrSchema)
		}
		from := locations[location].pos
		to := from
		for remaining := length; remaining > 0; location++ {
			if location == len(locations) || locations[location].pos != to {
				return nil, fmt.Errorf("%w: Yjs text does not align with ProseMirror text runs", ErrSchema)
			}
			width := locations[location].end - locations[location].pos
			if width > remaining {
				return nil, fmt.Errorf("%w: Yjs text splits a ProseMirror text run", ErrSchema)
			}
			to = locations[location].end
			remaining -= width
		}
		ranges = append(ranges, yTextRange{
			text:      current.text,
			textblock: current.textblock,
			Range:     Range{From: from, To: to},
		})
	}
	if location != len(locations) {
		return nil, fmt.Errorf("%w: ProseMirror text has no Yjs text", ErrSchema)
	}
	return ranges, nil
}

// yFragmentLocations appends each Yjs text among frag's children, which stand depth levels below
// the document, to texts with the textblock holding it. When locations is not nil, it also appends
// the range of each text node walk would visit among them in the Node shape the fragment reads as,
// with each text's delta operations as text nodes and each element as a node of its name and no
// text, where the children start at ProseMirror position pos, and returns their size as nodeSize
// counts it. It sizes and enters elements by walk's own rules (isOpaqueNodeType, opaqueNodeSize,
// boundaryTokens): an element named text is a text run of no width, and no Yjs text inside it or
// a leaf-named element is located. It refuses a node past MaxTreeDepth as Read does: an element,
// or a text holding anything, which Read makes a text node of. Its refusals, met in document order
// during the one traversal, precede any refusal from aligning texts with locations.
func yFragmentLocations(frag *crdt.YXmlFragment, textblock string, depth, pos int, texts *[]yTextNode, locations *[]textLocation) (int, error) {
	start := pos
	for _, child := range frag.Children() {
		switch current := child.(type) {
		case *crdt.YXmlText:
			if current.Len() > 0 {
				if err := treeDepthError(depth); err != nil {
					return 0, err
				}
			}
			*texts = append(*texts, yTextNode{text: current, textblock: textblock})
			delta, err := yTextDeltaInTransaction(current)
			if err != nil {
				return 0, err
			}
			for _, operation := range delta {
				value, ok := operation.Insert.(string)
				if !ok {
					return 0, fmt.Errorf("%w: unsupported Yjs text embed %T", ErrSchema, operation.Insert)
				}
				// Outside located children no position is read, so their widths go uncounted.
				if locations != nil {
					width := len16(value)
					*locations = append(*locations, textLocation{pos: pos, end: pos + width})
					pos += width
				}
			}
		case *crdt.YXmlElement:
			if err := treeDepthError(depth); err != nil {
				return 0, err
			}
			name := current.NodeName
			innerBlock := textblock
			switch name {
			case "paragraph", "heading", "code_block":
				innerBlock = name
			}
			opaque := isOpaqueNodeType(name)
			// walk visits an element named text as a text node holding no text, so it locates one of
			// no width, and enters it no more than a leaf.
			if name == "text" && locations != nil {
				*locations = append(*locations, textLocation{pos: pos, end: pos})
			}
			innerLocations := locations
			if opaque {
				innerLocations = nil
			}
			size, err := yFragmentLocations(&current.YXmlFragment, innerBlock, depth+1, pos+boundaryTokens(name)/2, texts, innerLocations)
			if err != nil {
				return 0, err
			}
			if opaque {
				pos += opaqueNodeSize(name, "")
			} else {
				pos += size + boundaryTokens(name)
			}
		default:
			return 0, fmt.Errorf("%w: unexpected Yjs child %T", ErrSchema, child)
		}
	}
	return pos - start, nil
}

// Unmark removes every markType mark whose id attribute equals id.
func Unmark(txn *crdt.Transaction, frag *crdt.YXmlFragment, markType, id string) error {
	if txn == nil || frag == nil {
		return fmt.Errorf("%w: Unmark requires transaction and fragment", ErrSchema)
	}
	if !active.marks[markType] {
		return fmt.Errorf("%w: mark %q", ErrSchema, markType)
	}

	spans, err := yTextRanges(frag)
	if err != nil {
		return err
	}
	found := false
	for _, span := range spans {
		operations, err := yTextDeltaInTransaction(span.text)
		if err != nil {
			return err
		}
		offset := 0
		for _, operation := range operations {
			value, ok := operation.Insert.(string)
			if !ok {
				return fmt.Errorf("%w: unsupported Yjs text embed %T", ErrSchema, operation.Insert)
			}
			for attributeName, attributeValue := range operation.Attributes {
				if yattrToMarkName(attributeName) == markType && markAttributeID(attributeValue) == id {
					span.text.Format(txn, offset, len16(value), crdt.Attributes{attributeName: nil})
					found = true
				}
			}
			offset += len16(value)
		}
	}
	if !found {
		return ErrTargetNotFound
	}
	return nil
}

func markAttributeID(value any) string {
	attrs, err := attrsFromY(value)
	if err != nil {
		return ""
	}
	id, _ := attrs["id"].(string)
	return id
}

// StripAnchorMarks is a copy of node without the marks the rendering does not write
// (renderedMarkTypes) - Proof and Dispatch anchors - and with adjacent text runs under equal
// written marks merged into one that keeps the first run's attributes and children. That is what
// the rendering reads (writtenTree), with lineBreaks false, so no line break is rewritten, and
// before any U+0000 is written (nulsWrittenTree): the text and the attributes are node's own
// bytes. The copy is the caller's, which cloneNode gives whether or not the rendering writes
// anything otherwise, since callers own and change what they get (docs.asUploaded, the paragraph
// a docs edit builds, spreadDifferences).
func StripAnchorMarks(node *Node) *Node {
	if node == nil {
		return nil
	}
	return cloneNode(writtenTree(node, false))
}
