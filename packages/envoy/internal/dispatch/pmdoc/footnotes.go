package pmdoc

import (
	"math/bits"
	"math/rand/v2"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// CalledFootnoteLabels is the label of every footnote reference inside block, each once, in
// document order and as the reference holds it, but for a label a footnote definition inside
// block defines, by the label or up to case (FootnoteLabelKey): block's own markdown holds that
// definition, and its reading resolves the reference to it as the document's does. Each label
// left is one the document defines outside block, so block's markdown read apart from the
// document reads a call of it as text unless a definition of it stands beside that markdown
// (WithStandInDefinitions).
//
// It walks block once, so its time grows with the calls block holds, not with their square: a
// set of the labels called so far leaves out a repeat, byte for byte, as the walk meets it, and a
// set of the keys of block's own definitions leaves out after the walk each label called whose key
// is among them, every label folded by the one keyer the reading makes (footnoteKeyer).
func CalledFootnoteLabels(block *Node) []string {
	var (
		called        []string
		seen, defined map[string]struct{}
		keyer         footnoteKeyer
	)
	Walk(block, func(node *Node) bool {
		if node.Type != "footnote_reference" && node.Type != "footnote_definition" {
			return true
		}
		label, ok := node.Attrs["label"].(string)
		switch {
		case !ok:
		case node.Type == "footnote_definition":
			if defined == nil {
				defined = map[string]struct{}{}
			}
			defined[keyer.key(label)] = struct{}{}
		default:
			if _, repeated := seen[label]; !repeated {
				if seen == nil {
					seen = map[string]struct{}{}
				}
				seen[label] = struct{}{}
				called = append(called, label)
			}
		}
		return true
	})
	if len(defined) == 0 {
		return called
	}
	return slices.DeleteFunc(called, func(label string) bool {
		_, own := defined[keyer.key(label)]
		return own
	})
}

// FootnoteDefinitionLabels is the label of every footnote definition in doc, each once, in
// document order and as the definition holds it.
func FootnoteDefinitionLabels(doc *Node) []string {
	var defined []string
	seen := map[string]struct{}{}
	Walk(doc, func(node *Node) bool {
		if node.Type != "footnote_definition" {
			return true
		}
		label, ok := node.Attrs["label"].(string)
		if _, repeated := seen[label]; ok && !repeated {
			seen[label] = struct{}{}
			defined = append(defined, label)
		}
		return true
	})
	return defined
}

// FootnoteLabelKeys is the labels a document defines, as FootnoteDefinitionLabels gives them, in
// document order, each kept under its key as a call writes it, and each key under its byte length
// and hash (footnoteKeyHash), made once for the document (KeyFootnoteLabels), so that the reading
// of each block of that document (Written) folds only the calls the block writes and finds the key
// a call folds to by the length and hash of its fold, at a cost the number of keys does not change.
type FootnoteLabelKeys struct {
	labels []string
	// keys is each key once, with the labels under it, in the order of the first label under each.
	keys []footnoteKey
	// byHash is, for the byte length and hash of each key, the index in keys of the last key with
	// them, from which sameHash reaches the others.
	byHash map[footnoteKeyHash]int
	// longest is the byte length of the longest key.
	longest int
}

// footnoteKey is a key of a document's labels with the index in those labels of each label under
// it: first, that of the first of them, and later, in order, those of the others, which equal it
// only up to case, since FootnoteDefinitionLabels keeps apart labels that are not equal byte for
// byte. sameHash is the index in the document's keys of the key before it with its byte length and
// hash, or -1 where none has them: two keys share both only by chance (footnoteHashBase).
type footnoteKey struct {
	key      string
	first    int
	later    []int
	sameHash int
}

// footnoteKeyHash is the byte length of a folded string and its hash: the polynomial whose
// coefficients are the string's bytes, the first byte the constant, taken at footnoteHashBase
// modulo footnoteHashModulus. The hash of a string with a byte before it is the string's hash
// times the base, plus the byte (footnoteHashStep), so a reading that walks a fold backwards has
// the hash of each suffix it has read with one step for each byte. Two strings of one length n
// that differ are two polynomials that differ, so they share a hash at n−1 of the bases at most.
type footnoteKeyHash struct {
	length int
	hash   uint64
}

// footnoteHashModulus is the Mersenne prime 2^61 − 1, modulo which footnoteKeyHash takes hashes.
const footnoteHashModulus = 1<<61 - 1

// footnoteHashBase is the base of footnoteKeyHash, drawn once for the process at random from 2 to
// footnoteHashModulus − 1, so that no document can be written whose calls fold to strings sharing
// a key's hash and not its bytes but by chance: a string of n bytes shares the hash of a key of
// its length at n−1 of the bases at most, a chance below n in 2^61.
var footnoteHashBase = 2 + rand.Uint64N(footnoteHashModulus-2)

// footnoteHashStep is the hash (footnoteKeyHash) of a string with b before it, where hash is the
// string's: hash times footnoteHashBase, plus b, modulo footnoteHashModulus. The product of two
// numbers under 2^61 is under 2^122, and 2^61 is 1 modulo the modulus, so the product's bits from
// the 61st up, added to the 61 bits under them, are the product modulo the modulus but for at most
// twice the modulus more.
func footnoteHashStep(hash uint64, b byte) uint64 {
	hi, lo := bits.Mul64(hash, footnoteHashBase)
	hash = (hi<<3 | lo>>61) + lo&footnoteHashModulus + uint64(b)
	for hash >= footnoteHashModulus {
		hash -= footnoteHashModulus
	}
	return hash
}

// footnoteHash is the hash of key (footnoteKeyHash), read from its end, as a reading of a block's
// calls reads their folds (footnoteCalls).
func footnoteHash(key string) uint64 {
	var hash uint64
	for index := len(key) - 1; index >= 0; index-- {
		hash = footnoteHashStep(hash, key[index])
	}
	return hash
}

// KeyFootnoteLabels is the FootnoteLabelKeys of labels, the labels a document defines as
// FootnoteDefinitionLabels gives them: each label under the key of the label as a call writes it,
// written as the renderer writes one (escapeFootnoteLabel) and folded (FootnoteLabelKey), two
// labels equal only up to case under the same key, each key under its byte length and hash
// (footnoteKeyHash), and the byte length of the longest key. It folds and hashes each label once,
// and is made once for the document, each block of which is read against it (Written).
func KeyFootnoteLabels(labels []string) FootnoteLabelKeys {
	keys := FootnoteLabelKeys{
		labels: labels,
		keys:   make([]footnoteKey, 0, len(labels)),
		byHash: make(map[footnoteKeyHash]int, len(labels)),
	}
	var keyer footnoteKeyer
	for index, label := range labels {
		key := keyer.key(escapeFootnoteLabel(label))
		keys.longest = max(keys.longest, len(key))
		at := footnoteKeyHash{length: len(key), hash: footnoteHash(key)}
		last, ok := keys.byHash[at]
		if !ok {
			last = -1
		}
		held := last
		for held >= 0 && keys.keys[held].key != key {
			held = keys.keys[held].sameHash
		}
		if held >= 0 {
			keys.keys[held].later = append(keys.keys[held].later, index)
			continue
		}
		keys.byHash[at] = len(keys.keys)
		keys.keys = append(keys.keys, footnoteKey{key: key, first: index, sameHash: last})
	}
	return keys
}

// Written is each of the labels of keys that markdown writes as a footnote call, `[^label]`, the
// label written as the renderer writes one (escapeFootnoteLabel) and compared up to case
// (FootnoteLabelKey), as a reference resolves to a definition whose label matches it only up to
// case: each label once, in the order of the labels, which is the order the document defines them
// in, and nil where markdown writes none. It reads markdown's characters, not its parse, so it also
// answers a label markdown writes in a call that is escaped or in code, which reads as text, and
// one markdown defines itself, whose calls resolve to that definition. A stand-in definition of
// either after markdown (WithStandInDefinitions) is one nothing in markdown refers to.
//
// It reads markdown in one pass. Each `[^` opens a call whose label starts after the caret,
// wherever the `[^` stands, after a backslash or inside the label of another call too. A backslash
// takes the byte after it, so a `]` it takes closes nothing, and the first `]` no backslash takes
// closes every call open, each label running up to that `]`. A label of more than four bytes for
// each byte of the longest key folds to no key, since a character is at most four bytes and folds
// to one byte at least, so the labels a `]` closes that can fold to a key are the last ones opened
// within that bound, each the end of the label of the oldest of them. The reading folds that oldest
// label once, which folds each of the others with it, and walks the fold backwards from the `]`
// once, hashing each byte, so that at the start of each label it holds the hash of the label's fold
// and looks the fold up among the keys by its byte length and hash, until the fold grows longer
// than the longest key (footnoteCalls). It takes a key found so only where the fold equals it byte
// for byte, so the answer is exact, and compares no key it has taken already, since that adds
// nothing.
//
// That answers as a search of markdown folded for each label's call folded does: the renderer
// writes each `]` of a label after an odd run of backslashes and doubles each backslash that ends
// the label, so a label's call runs from its `[^` to the first `]` no backslash takes; and folding
// maps markdown character by character and makes or removes no `[`, `^`, `]` or backslash, so the
// fold of a call's label is the same span of markdown folded.
//
// Its time grows with markdown's length, whatever labels the document defines, and with one
// comparison for each key it answers. A `]` closes every call open, so the spans two of them fold
// do not overlap: each byte of markdown is folded once at most, each byte of a fold is hashed once
// at most, and each label is looked up once at most, at a cost the number of keys does not change.
// The comparisons cost the bytes of each key the reading takes, once, and those of a fold that
// shares a key's length and hash and not its bytes, which the base drawn at random for the process
// leaves to chance (footnoteHashBase). Every fold goes into one buffer the reading keeps, which a
// fold all ASCII never outgrows and fills with nothing allocated, so the reading of a block all
// ASCII makes no more allocations against the keys of the labels it calls among thousands of
// others than against those keys alone.
func (keys FootnoteLabelKeys) Written(markdown string) []string {
	first := strings.Index(markdown, "[^")
	if len(keys.labels) == 0 || first < 0 {
		return nil
	}
	bound := 4 * keys.longest
	// No label within the bound runs longer than the buffer, nor than markdown after its first
	// `[^`, and a label all ASCII folds to as many bytes as it holds.
	calls := footnoteCalls{keys: &keys, folded: make([]byte, 0, min(bound, len(markdown)-first))}
	var open []int
	for index := first; index < len(markdown); index++ {
		if markdown[index] == '\\' {
			// The byte a backslash takes closes nothing, but the bracket of a `[^` still opens a
			// call.
			index++
			if !strings.HasPrefix(markdown[index:], "[^") {
				continue
			}
		}
		switch {
		case strings.HasPrefix(markdown[index:], "[^"):
			open = append(open, index+2)
			index++
		case markdown[index] == ']':
			// The calls opened last have the shortest labels, so those within the bound are the
			// last ones opened.
			oldest := len(open)
			for oldest > 0 && index-open[oldest-1] <= bound {
				oldest--
			}
			if oldest < len(open) {
				calls.close(markdown, open[oldest:], index)
			}
			open = open[:0]
		}
	}
	if len(calls.called) == 0 {
		return nil
	}
	// Each key is taken once, and each label is under one key, so no label repeats.
	slices.Sort(calls.called)
	labels := make([]string, len(calls.called))
	for each, index := range calls.called {
		labels[each] = keys.labels[index]
	}
	return labels
}

// footnoteCalls is one reading of a block's calls against the keys of its document's labels
// (FootnoteLabelKeys.Written): the keyer that folds the calls' labels, the buffer the labels each
// `]` closes are folded into, the keys the reading has taken, by their index among the keys, and
// the index in the document's labels of each label under them.
type footnoteCalls struct {
	keys   *FootnoteLabelKeys
	keyer  footnoteKeyer
	folded []byte
	taken  map[int]struct{}
	called []int
}

// close takes the keys that the labels a `]` at end closes fold to, of the labels markdown opens
// at starts, in order, each the end of the first one's. It folds the first label once into the
// buffer, a piece from the start of each label to that of the next and the last to end, which
// folds each label whole, since folding maps a label character by character and each piece but the
// last ends with a caret, and keeps in starts where the fold of each label starts in the buffer. It
// then walks the fold backwards from its end once, hashing each byte (footnoteHashStep), so that at
// the start of each label the hash is that of the label's fold, and looks up the fold's byte length
// and hash among the keys, up to the first label whose fold is longer than the longest key, which
// folds to no key, as do the longer ones before it. A key found so is taken where it equals the
// fold byte for byte, and not compared where the reading has taken it already.
func (calls *footnoteCalls) close(markdown string, starts []int, end int) {
	keys := calls.keys
	calls.folded = calls.folded[:0]
	for each, start := range starts {
		stop := end
		if each+1 < len(starts) {
			stop = starts[each+1]
		}
		starts[each] = len(calls.folded)
		calls.folded = calls.keyer.appendKey(calls.folded, markdown[start:stop])
	}
	folded := calls.folded
	var hash uint64
	read := len(folded)
	for each := len(starts) - 1; each >= 0 && len(folded)-starts[each] <= keys.longest; each-- {
		start := starts[each]
		for ; read > start; read-- {
			hash = footnoteHashStep(hash, folded[read-1])
		}
		held, ok := keys.byHash[footnoteKeyHash{length: len(folded) - start, hash: hash}]
		for ; ok && held >= 0; held = keys.keys[held].sameHash {
			key := &keys.keys[held]
			if _, done := calls.taken[held]; done || string(folded[start:]) != key.key {
				continue
			}
			if calls.taken == nil {
				calls.taken = map[int]struct{}{}
			}
			calls.taken[held] = struct{}{}
			calls.called = append(append(calls.called, key.first), key.later...)
			break
		}
	}
}

// footnoteKeyer makes the keys of footnote labels (FootnoteLabelKey) with one caser lowering and
// one uppercasing, which it makes on the first label it needs them for: a label all ASCII has its
// ASCII upper case as its key, which is what lowering and then uppercasing it under language.Und
// makes of it. A caser holds state while it maps, so a keyer is not safe for concurrent use, and
// each reading makes its own.
type footnoteKeyer struct {
	lower, upper cases.Caser
	made         bool
}

// key is label's FootnoteLabelKey.
func (keyer *footnoteKeyer) key(label string) string {
	if !allASCII(label) {
		return keyer.caseKey(label)
	}
	return strings.ToUpper(label)
}

// appendKey is dst with label's FootnoteLabelKey appended: the ASCII upper case of a label all
// ASCII, appended byte by byte, which allocates nothing while dst has room, and what the casers
// make of any other label (caseKey).
func (keyer *footnoteKeyer) appendKey(dst []byte, label string) []byte {
	if !allASCII(label) {
		return append(dst, keyer.caseKey(label)...)
	}
	for index := range len(label) {
		char := label[index]
		if 'a' <= char && char <= 'z' {
			char -= 'a' - 'A'
		}
		dst = append(dst, char)
	}
	return dst
}

// caseKey is label lowered and then uppercased by the keyer's casers, which it makes on the first
// label that needs them.
func (keyer *footnoteKeyer) caseKey(label string) string {
	if !keyer.made {
		keyer.lower, keyer.upper, keyer.made = cases.Lower(language.Und), cases.Upper(language.Und), true
	}
	return keyer.upper.String(keyer.lower.String(label))
}

// allASCII reports whether every byte of value is ASCII.
func allASCII(value string) bool {
	for index := range len(value) {
		if value[index] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// WithStandInDefinitions is markdown with a stand-in definition of each of labels after it: for
// each label, a blank line and `[^label]: x`, the label written as the renderer writes one
// (escapeFootnoteLabel), so every reader reads it back as that label. A reading of it reads each
// call of those labels in markdown as a reference, where markdown read alone reads it as text:
// the read-back of a run the renderer wrote (parseInlineWithDefinitions) and the display tree of
// a block's markdown cut out of the document that defines the labels both read markdown so. The
// stand-ins are the last blocks of that reading, one footnote definition each, unless markdown
// ends in a block a definition's line continues. With no labels it is markdown.
func WithStandInDefinitions(markdown string, labels []string) string {
	if len(labels) == 0 {
		return markdown
	}
	size := len(markdown)
	for _, label := range labels {
		size += len("\n\n[^]: x") + len(label)
	}
	var full strings.Builder
	full.Grow(size)
	full.WriteString(markdown)
	for _, label := range labels {
		full.WriteString("\n\n[^")
		full.WriteString(escapeFootnoteLabel(label))
		full.WriteString("]: x")
	}
	return full.String()
}

// footnoteReferenceParser reads a footnote reference, `[^label]`, or `![^label]` after an image's
// bang, and resolves its label from maps of the parse's definitions built once
// (footnoteDefinitionsOf): a label equal byte for byte to a definition's resolves to the first such
// definition, as goldmark's footnote parser resolves it, and one that matches a definition's only
// up to case (FootnoteLabelKey) to the first definition with that key, as the browser editor's
// parser resolves it, where goldmark read `[^A]` as text beside `[^a]: `. Such a reference carries
// its label as written (referenceLabelAttr), which the browser editor keeps on it. It is a parser
// of its own, not goldmark's wrapped: goldmark's found the definition by walking every one of them
// for each reference, so a body of n references and their n definitions cost it n²/2 comparisons,
// and after a bang it read the third byte as the caret without asking whether the second opened
// the label, so `!x^a]` beside `[^a]: ` lost its `x` to a reference and every `!x^` scanned the
// rest of its line for a closer.
type footnoteReferenceParser struct{}

// Trigger is the bytes a reference starts with: the label's opener, or an image's bang before it.
func (footnoteReferenceParser) Trigger() []byte { return []byte{'!', '['} }

// FootnoteLabelKey is a footnote label as the browser editor's parser compares it: with full case
// mapping, lowered and then uppercased, so `ß` matches `SS` and `İ` matches `i̇`, which simple case
// folding keeps apart. It folds label through a keyer of its own (footnoteKeyer), so the fold is
// written once; a reading that keys many labels keeps one keyer for all of them.
func FootnoteLabelKey(label string) string {
	var keyer footnoteKeyer
	return keyer.key(label)
}

// referenceLabelAttr is the label a reference resolved by footnoteReferenceParser only up to case
// is written with.
var referenceLabelAttr = []byte("pmdoc-reference-label")

func (footnoteReferenceParser) Parse(parent ast.Node, block gmtext.Reader, pc parser.Context) ast.Node {
	line, segment := block.PeekLine()
	opener := 0
	if len(line) > 0 && line[0] == '!' {
		opener++
	}
	// start is the caret's index, after the opener.
	start := opener + 1
	if start+1 >= len(line) || line[opener] != '[' || line[start] != '^' {
		return nil
	}
	closure := util.FindClosure(line[start+1:], '[', ']', false, false) //nolint:staticcheck
	if closure < 0 {
		return nil
	}
	label := line[start+1 : start+1+closure]
	definitions := footnoteDefinitionsOf(pc)
	// Goldmark compares the label as its reader gives the segment's value back.
	written := block.Value(gmtext.NewSegment(segment.Start+start+1, segment.Start+start+1+closure))
	definition, exact := definitions.byLabel[string(written)], true
	if definition == nil {
		definition, exact = definitions.byKey[FootnoteLabelKey(string(label))], false
	}
	if definition == nil {
		return nil
	}
	if definition.Index < 0 {
		list := definition.Parent().(*extensionast.FootnoteList)
		list.Count++
		definition.Index = list.Count
	}
	block.Advance(start + 1 + closure + 1)
	link := extensionast.NewFootnoteLink(definition.Index)
	if !exact {
		link.SetAttribute(referenceLabelAttr, string(label))
	}
	if line[0] == '!' {
		parent.AppendChild(parent, ast.NewTextSegment(gmtext.NewSegment(segment.Start, segment.Start+1)))
	}
	return link
}

// footnoteDefinitions is each footnote definition in goldmark's list by its label as written
// (byLabel) and by its label's key (byKey, FootnoteLabelKey), the first in the list where two share
// one: goldmark's list holds them in the order they close, which is the order of their slots.
type footnoteDefinitions struct {
	byLabel, byKey map[string]*extensionast.Footnote
}

// footnoteKeysKey holds a parse's footnoteDefinitionsOf.
var footnoteKeysKey = parser.NewContextKey()

// footnoteDefinitionsOf is the parse's footnoteDefinitions. It is built once per parse, on its
// first reference: inline text is parsed after every block has closed, so each definition has its
// slot by then.
func footnoteDefinitionsOf(pc parser.Context) footnoteDefinitions {
	if definitions, ok := pc.Get(footnoteKeysKey).(footnoteDefinitions); ok {
		return definitions
	}
	slots, _ := pc.Get(footnoteSlotsKey).([]*footnoteSlot)
	definitions := footnoteDefinitions{
		byLabel: make(map[string]*extensionast.Footnote, len(slots)),
		byKey:   make(map[string]*extensionast.Footnote, len(slots)),
	}
	for _, slot := range slots {
		definition, ok := slot.definition.(*extensionast.Footnote)
		if !ok {
			continue
		}
		if _, listed := definition.Parent().(*extensionast.FootnoteList); !listed {
			continue
		}
		if _, seen := definitions.byLabel[string(definition.Ref)]; !seen {
			definitions.byLabel[string(definition.Ref)] = definition
		}
		key := FootnoteLabelKey(string(definition.Ref))
		if _, seen := definitions.byKey[key]; !seen {
			definitions.byKey[key] = definition
		}
	}
	pc.Set(footnoteKeysKey, definitions)
	return definitions
}

// footnotes is goldmark's footnote extension with its definition parser taking every space and
// tab after a definition's `]:` as part of its prefix, as the browser editor's parser does, so the
// definition's first block starts at the column its later lines are measured from: goldmark left
// them to that block, which read a fence opening the definition as indented by one (and dropped a
// space from each code line) and a list's marker one column in (which put its item's later blocks
// outside it).
type footnotes struct{}

func (footnotes) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(footnoteParserOptions()...)
}

// footnoteParserOptions registers footnotes: the definition parser (footnoteDefinitionParser) and
// the reference parser (footnoteReferenceParser), which numbers the definitions it refers to.
// Goldmark's footnote transformer is left out: it only orders the definitions, drops those nothing
// refers to and appends backlinks, and the parser keeps every definition where it is written
// (definitionsInPlace) and writes no backlink.
func footnoteParserOptions() []parser.Option {
	return []parser.Option{
		parser.WithBlockParsers(util.Prioritized(frontmatterAttempt{endsContainers{footnoteDefinitionParser{nestingGuard{BlockParser: extension.NewFootnoteBlockParser()}}}}, 999)),
		parser.WithInlineParsers(util.Prioritized(footnoteReferenceParser{}, 101)),
	}
}

type footnoteDefinitionParser struct{ parser.BlockParser }

// footnoteSlot stands where a footnote definition is written while the document parses:
// goldmark's footnote extension moves each definition it closes into the list it gathers at the
// document's end, and its transformer drops the ones nothing refers to. definitionsInPlace puts
// each back in its slot, where the browser editor's parser keeps it.
type footnoteSlot struct {
	ast.BaseBlock
	definition ast.Node
}

var (
	kindFootnoteSlot = ast.NewNodeKind("FootnoteSlot")
	footnoteSlotsKey = parser.NewContextKey()
)

func (s *footnoteSlot) Kind() ast.NodeKind { return kindFootnoteSlot }

func (s *footnoteSlot) Dump(source []byte, level int) { ast.DumpHelper(s, source, level, nil, nil) }

// Close leaves a slot where the definition is written and hands goldmark the definition from the
// document's level, where goldmark puts the list it gathers definitions in when it closes the first
// one: a list put inside a definition would leave the document with the definition that holds it,
// and goldmark reads no text in blocks outside the document. That list goes ahead of the blocks
// written, where no check of the block before another (interruptsOpenBlock) meets it while the
// document parses: goldmark put it at the document's end, between the blocks written before and
// after the definition closed.
func (p footnoteDefinitionParser) Close(node ast.Node, reader gmtext.Reader, pc parser.Context) {
	slot := &footnoteSlot{definition: node}
	node.Parent().InsertBefore(node.Parent(), node, slot)
	slots, _ := pc.Get(footnoteSlotsKey).([]*footnoteSlot)
	pc.Set(footnoteSlotsKey, append(slots, slot))
	root := node.Parent()
	for root.Parent() != nil {
		root = root.Parent()
	}
	root.AppendChild(root, node)
	p.BlockParser.Close(node, reader, pc)
	if list := node.Parent(); list != root.FirstChild() {
		root.InsertBefore(root, root.FirstChild(), list)
	}
}

// definitionsInPlace puts each footnote definition back where it is written, in place of its slot,
// and removes the list goldmark gathered them in.
func definitionsInPlace(root ast.Node, context parser.Context) {
	slots, _ := context.Get(footnoteSlotsKey).([]*footnoteSlot)
	for _, slot := range slots {
		if parent := slot.definition.Parent(); parent != nil {
			parent.RemoveChild(parent, slot.definition)
		}
		slot.Parent().ReplaceChild(slot.Parent(), slot, slot.definition)
	}
	if list, ok := root.FirstChild().(*extensionast.FootnoteList); ok {
		root.RemoveChild(root, list)
	}
}

// Open reads the definition's opener without the padding a tab split by the containers' prefix
// leaves ahead of it: goldmark's parser measures where the definition's text starts from the line
// without that padding, so it would take the label's first characters as the text (`]: def`).
func (p footnoteDefinitionParser) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	_, segment := reader.PeekLine()
	offset := pc.BlockOffset()
	padded := segment.Padding > 0 && offset >= segment.Padding
	if padded {
		setPadding(reader, 0)
		pc.SetBlockOffset(offset - segment.Padding)
	}
	node, state := p.BlockParser.Open(parent, reader, pc)
	if node == nil {
		if padded {
			setPadding(reader, segment.Padding)
			pc.SetBlockOffset(offset)
		}
		return node, state
	}
	if state&parser.HasChildren != 0 {
		line, _ := reader.PeekLine()
		skip := 0
		for skip < len(line) && (line[skip] == ' ' || line[skip] == '\t') {
			skip++
		}
		if skip < len(line) && !util.IsBlank(line[skip:]) {
			reader.Advance(skip)
		}
	}
	return node, state
}
