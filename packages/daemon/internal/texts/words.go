package texts

import (
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// The bounds of carried words, in UTF-16 units, as Dispatch counts a body.
const (
	// OneAct is the bound of the words of one act, where a text carries one act.
	OneAct = 4000
	// EachOfSeveral is the bound of the words of each act, where a text carries several acts.
	EachOfSeveral = 1000
	// Summary is the bound of a worker's summary.
	Summary = 1000
)

// Words are the words of another author that a text carries in a block of their own.
type Words struct {
	// Text is the words as the event carried them, after the bound.
	Text string
	// By names the author. Set it only where the fields of the text do not name the author.
	By string
	// At is the time of the act. Set it only where the fields of the text do not give the time.
	At time.Time
	// Field is the path of the words in the payload, such as kept[1].answer, where the words
	// are a field of a record in a list. Render sets it for the Words of a list's records.
	Field string
	// Rest is the count of UTF-16 units that the bound cut off.
	Rest int
	// More is true where the event marked its body as cut, or reached the daemon as a
	// payload_omitted stand-in. Then the count of the rest is not known.
	More bool
}

// WordsMeta is what a notice's payload carries for one field that holds another author's
// words, in the payload's words object, keyed by the path of that field.
type WordsMeta struct {
	By   string    `json:"by,omitempty"`
	At   time.Time `json:"at,omitzero"`
	Rest int       `json:"rest,omitempty"`
	More bool      `json:"more,omitempty"`
}

// wordsTag is the struct tag of a string field of a notice or a role-message record that holds
// another author's words: texts:"words".
const wordsTag = "words"

// WordsPaths lists the path of each field of record that holds another author's words and is
// set: a string field tagged texts:"words" that is not empty, and a Words or *Words field that
// carries words, a cut or a mark that more follows. A path is the field's json name after the json
// names of the objects that hold it, joined by dots, with the index of a record in a list after the
// list's name, counted from 0: summary, kept[1].answer, catch_up.kept.decisions[1].answer. An
// embedded struct with no json tag gives its fields in its place, as encoding/json reads it. The
// paths come in the order of the fields' declaration.
//
// The words object of a notice's or a role message's payload is keyed by these paths, with a
// WordsMeta for each.
func WordsPaths(record any) []string {
	return wordsPathsIn(nil, reflect.ValueOf(record), "")
}

// wordsPathsIn appends the paths of the words fields that v holds at path: v's own fields for a
// struct, the fields of each record for a list, and nothing for any other value.
func wordsPathsIn(paths []string, v reflect.Value, path string) []string {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return paths
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == timeType {
			return paths
		}
		return wordsPathsOf(paths, v, path)
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			paths = wordsPathsIn(paths, v.Index(i), path+"["+strconv.Itoa(i)+"]")
		}
	}
	return paths
}

// wordsPathsOf appends the paths of the words fields of the struct v, whose own path is path
// (empty for the record itself).
func wordsPathsOf(paths []string, v reflect.Value, path string) []string {
	for i := range v.NumField() {
		field := v.Type().Field(i)
		tag, tagged := field.Tag.Lookup("json")
		if tag == "-" || !field.IsExported() {
			continue
		}
		value := v.Field(i)
		if field.Anonymous && !tagged {
			paths = wordsPathsIn(paths, value, path)
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = field.Name
		}
		if path != "" {
			name = path + "." + name
		}
		switch {
		case field.Type == wordsType:
			if !value.Interface().(Words).empty() {
				paths = append(paths, name)
			}
		case field.Type == wordsPtrType:
			if !value.IsNil() && !value.Elem().Interface().(Words).empty() {
				paths = append(paths, name)
			}
		case field.Tag.Get("texts") == wordsTag:
			if value.Kind() == reflect.String && value.Len() > 0 {
				paths = append(paths, name)
			}
		default:
			paths = wordsPathsIn(paths, value, name)
		}
	}
	return paths
}

// Bound cuts text to its first limit UTF-16 units. It returns that head and the count of
// UTF-16 units it cut off. A character outside the Basic Multilingual Plane counts two units,
// and Bound never splits it: where its two units would cross the limit, the head leaves it out.
// A byte that is not valid UTF-8 decodes as U+FFFD, one unit, as a JSON decoder reads it.
func Bound(text string, limit int) (head string, rest int) {
	units := 0
	for i, r := range text {
		n := utf16.RuneLen(r)
		if units+n > limit {
			return text[:i], utf16Length(text[i:])
		}
		units += n
	}
	return text, 0
}

// Carry bounds text to limit and gives the Words that a text carries. Where the event marked
// its body as cut (more), the Words say that more follows and give no count of the rest.
// Otherwise they give the count of the UTF-16 units that the bound cut off. The caller sets By
// and At where the fields of the text do not name them.
func Carry(text string, limit int, more bool) Words {
	head, rest := Bound(text, limit)
	if more {
		return Words{Text: head, More: true}
	}
	return Words{Text: head, Rest: rest}
}

// Block draws w in a block of its own, the block that carries another author's words after a
// text's fields (dispatch://INFRA-3686/spec R1), as the whole of its text: its tag is the one
// blockTags gives a block with nothing before it and no other block beside it. The opening tag
// carries the attributes by, at, field, rest and more, in that order, each only where it is set.
// Then come a new line, the words exactly as given, a new line and the closing tag. Render draws
// the blocks of a text together (drawBlocks), since the tag of each is chosen against the lines
// before them and the words of them all.
func Block(name string, w Words) string {
	return drawBlock(blockTags(nil, []block{{name: name, words: w}})[0], w)
}

// block is one block of a text: the name of the field that holds the words, and the words.
type block struct {
	name  string
	words Words
}

// drawBlocks draws the blocks of one text, in their order, each under the tag blockTags gives it
// against before, the lines of the text ahead of the blocks.
func drawBlocks(before []string, blocks []block) []string {
	tags := blockTags(before, blocks)
	drawn := make([]string, len(blocks))
	for i, b := range blocks {
		drawn[i] = drawBlock(tags[i], b.words)
	}
	return drawn
}

// InFront is parts and then text, each joined to the next by a blank line: the notes a start puts
// in front of a task (supervise's Delivery.message), and the task. A part can hold words that are
// not the daemon's: a process's cause quotes the runtime's, and those can carry a tail of the
// role's own log, a task's own lines among it. So text's blocks (drawnBlocks) are drawn again,
// each with its attributes and its words as they were, under the tag blockTags gives it against
// the whole of what is sent: the parts, the lines of text outside its blocks and the words of every
// block. Nothing in front of a block then opens or closes its tag, as nothing does in the task
// alone (Render). Each block keeps the tag Render gave it wherever the parts hold none of those
// tags, and the parts and a text with no block are joined as they are.
func InFront(parts []string, text string) string {
	if len(parts) == 0 {
		return text
	}
	front := strings.Join(parts, "\n\n") + "\n\n"
	found := drawnBlocks(text)
	if len(found) == 0 {
		return front + text
	}
	head, tail := text[:found[0].start], text[found[len(found)-1].end:]
	before := make([]string, 0, len(parts)+2)
	before = append(append(before, parts...), head, tail)
	blocks := make([]block, len(found))
	for i, d := range found {
		blocks[i] = block{name: d.name(), words: Words{Text: d.words}}
	}
	tags := blockTags(before, blocks)
	var b strings.Builder
	b.Grow(len(front) + len(text) + 8*len(found))
	b.WriteString(front)
	b.WriteString(head)
	for i, d := range found {
		if i > 0 {
			b.WriteByte('\n')
		}
		// The block as drawBlock draws it, its opening tag's attributes as that tag wrote them.
		b.WriteString("<")
		b.WriteString(tags[i])
		b.WriteString(d.attributes)
		b.WriteString(">\n")
		b.WriteString(d.words)
		b.WriteString("\n</")
		b.WriteString(tags[i])
		b.WriteString(">")
	}
	b.WriteString(tail)
	return b.String()
}

// drawnOpening is the opening tag of a block as drawBlock draws it, a line of its own: the tag,
// then each attribute as writeAttribute writes it, whose value holds no double quote and no angle
// bracket, since writeAttribute escapes all three. It is the TypeScript reader's rule too
// (OPENING, packages/envoy-client/src/words-block.ts), and testdata/in-front.json holds the two
// to one output: a line whose value holds a raw <, > or " opens no block for either.
var drawnOpening = regexp.MustCompile(`^<([A-Za-z_][A-Za-z0-9_]*(?:-[1-9][0-9]*)?)((?: [a-z]+="[^"<>]*")*)>$`)

// drawnBlock is one block of a drawn text: where its opening line starts and where its closing
// tag ends, its tag, the attributes of its opening tag as written, and its words.
type drawnBlock struct {
	start, end int
	tag        string
	attributes string
	words      string
}

// name is the name of the field whose words the block holds: its tag without the -n that a step
// past held tags added (blockTags).
func (d drawnBlock) name() string {
	if at := strings.LastIndexByte(d.tag, '-'); at >= 0 {
		return d.tag[:at]
	}
	return d.tag
}

// drawnBlocks is the blocks of text, a text Render drew, in their order. No line of a drawn text
// ahead of its blocks starts with <, since a field is a TOON line and the lead and the kept
// sentences are the daemon's own, so its first such line opens its first block. A block closes on
// the first line after its opening that is its closing tag, since no words of a block hold that
// tag (blockTags), and the next block opens on the line after. The run ends at the first line that
// opens no block. It is empty where the first line that starts with < opens none, or opens a block
// that never closes, as in a text Render did not draw, such as an operator's task, which InFront
// then joins as it is. One pass over text.
func drawnBlocks(text string) []drawnBlock {
	at := 0
	if !strings.HasPrefix(text, "<") {
		line := strings.Index(text, "\n<")
		if line < 0 {
			return nil
		}
		at = line + 1
	}
	var blocks []drawnBlock
	for at < len(text) && text[at] == '<' {
		opened := strings.IndexByte(text[at:], '\n')
		if opened < 0 {
			break
		}
		match := drawnOpening.FindStringSubmatch(text[at : at+opened])
		if match == nil {
			break
		}
		wordsAt := at + opened + 1
		closing := "\n</" + match[1] + ">"
		closed := strings.Index(text[wordsAt:], closing)
		if closed < 0 {
			break
		}
		end := wordsAt + closed + len(closing)
		if end < len(text) && text[end] != '\n' {
			break
		}
		blocks = append(blocks, drawnBlock{start: at, end: end, tag: match[1], attributes: match[2], words: text[wordsAt : wordsAt+closed]})
		at = end + 1
	}
	return blocks
}

// drawBlock draws w under tag.
func drawBlock(tag string, w Words) string {
	var b strings.Builder
	b.Grow(len(tag)*2 + len(w.Text) + 64)
	b.WriteString("<")
	b.WriteString(tag)
	if w.By != "" {
		writeAttribute(&b, "by", w.By)
	}
	if !w.At.IsZero() {
		writeAttribute(&b, "at", formatTime(w.At))
	}
	if w.Field != "" {
		writeAttribute(&b, "field", w.Field)
	}
	if w.Rest > 0 {
		writeAttribute(&b, "rest", strconv.Itoa(w.Rest))
	}
	if w.More {
		writeAttribute(&b, "more", "true")
	}
	b.WriteString(">\n")
	b.WriteString(w.Text)
	b.WriteString("\n</")
	b.WriteString(tag)
	b.WriteString(">")
	return b.String()
}

// empty says whether w carries nothing at all: no words, no cut and no mark that more follows.
func (w Words) empty() bool {
	return w.Text == "" && w.Rest == 0 && !w.More
}

// blockTags is the tag of each of blocks, the blocks of one text in their order, after the lines
// before: the first of the block's name, name-1, name-2 and on that neither a line before the
// blocks nor the words of any block of the text hold as <tag or </tag, and that no earlier block
// took. The lines before are the whole text ahead of the first block, its lead, its fields and its
// kept sentences, which can hold another author's words too: an issue's title, a check's name, an
// option of an ask. So nothing in the text opens or closes the tag of a block but that block's own
// lines, and no two blocks share a tag: a reader that pairs each opening tag with the next closing
// tag of its name reads each block's words as its own. The test is a plain substring test, so
// text that holds <name-1 or <names also steps past name.
//
// The text is read once, for every name at once (tagNames), and each block's tag is then looked
// up in what that read holds, so a text is drawn in time linear in its length whatever the count
// of its names. A name's next block starts where its last block's tag stopped: every suffix below
// that one is held or taken, and stays so. limit, the length of the lines, the length of the
// words and the count of the blocks, bounds every tag the text can need, since each held number
// and each block takes away at most one candidate.
func blockTags(before []string, blocks []block) []string {
	limit := len(blocks)
	for _, line := range before {
		limit += len(line)
	}
	for _, b := range blocks {
		limit += len(b.words.Text)
	}
	names, ids := newTagNames(blocks, limit)
	for _, line := range before {
		names.hold(line)
	}
	for _, b := range blocks {
		names.hold(b.words.Text)
	}
	tags := make([]string, len(blocks))
	taken := make(map[string]bool, len(blocks))
	next := make([]int, len(names.held))
	for i, b := range blocks {
		id := ids[i]
		for n := next[id]; ; n++ {
			if names.held[id][n] {
				continue
			}
			tag := b.name
			if n > 0 {
				tag += "-" + strconv.Itoa(n)
			}
			if !taken[tag] {
				tags[i], taken[tag], next[id] = tag, true, n+1
				break
			}
		}
	}
	return tags
}

// tagNames is the names of a text's blocks, as a trie whose node 0 is the empty name, and what the
// text holds of each name's tags (hold).
type tagNames struct {
	// edges is the node each node's edge of one byte leads to.
	edges map[tagEdge]int
	// name is, for each node, one more than the index of the name that ends there, 0 where none
	// does.
	name []int
	// held is, for each name, each n up to limit whose tag a text read holds: name for n = 0 and
	// name-n otherwise. It is nil for a name whose tags no text read holds.
	held  []map[int]bool
	limit int
}

// tagEdge is an edge of the trie: the node it leaves and the byte it reads.
type tagEdge struct {
	from int
	b    byte
}

// newTagNames is the trie of the names of blocks, which holds nothing yet, and the index of each
// block's name in it: blocks of one name share one index.
func newTagNames(blocks []block, limit int) (*tagNames, []int) {
	names := &tagNames{edges: map[tagEdge]int{}, name: []int{0}, limit: limit}
	ids := make([]int, len(blocks))
	for i, b := range blocks {
		node := 0
		for j := range len(b.name) {
			edge := tagEdge{from: node, b: b.name[j]}
			child, ok := names.edges[edge]
			if !ok {
				child = len(names.name)
				names.name = append(names.name, 0)
				names.edges[edge] = child
			}
			node = child
		}
		if names.name[node] == 0 {
			names.held = append(names.held, nil)
			names.name[node] = len(names.held)
		}
		ids[i] = names.name[node] - 1
	}
	return names, ids
}

// hold adds what text holds of the names' tags. At each < of text, past a / that follows it, the
// names are walked along the text that follows: each name the walk reaches is one the text holds
// as <name or </name, so it is held at 0, and after <name- or </name- each number whose digits
// begin the digits there is held too (<name-12 holds 1 and 12). A name is a field's json name, of
// letters, digits and underscores, so a walk reads no further than the run of those after its <,
// and no two runs meet, since a run holds no <: text is read once. A text is read on its own,
// which finds what the joined text holds: a tag holds no new line.
func (t *tagNames) hold(text string) {
	for at := strings.IndexByte(text, '<'); at >= 0; {
		from := at + 1
		if from < len(text) && text[from] == '/' {
			from++
		}
		node := 0
		for i := from; ; i++ {
			if id := t.name[node]; id > 0 {
				t.holdSuffixes(id-1, text[i:])
			}
			if i == len(text) {
				break
			}
			child, ok := t.edges[tagEdge{from: node, b: text[i]}]
			if !ok {
				break
			}
			node = child
		}
		next := strings.IndexByte(text[at+1:], '<')
		if next < 0 {
			return
		}
		at += 1 + next
	}
}

// holdSuffixes holds the name of index id at 0, and, where after, the text after the name, is -
// and a number with no leading zero, each number up to limit that the number's digits begin.
func (t *tagNames) holdSuffixes(id int, after string) {
	held := t.held[id]
	if held == nil {
		held = map[int]bool{}
		t.held[id] = held
	}
	held[0] = true
	digits, suffixed := strings.CutPrefix(after, "-")
	if !suffixed || digits == "" || digits[0] < '1' || digits[0] > '9' {
		return
	}
	for n, j := 0, 0; j < len(digits) && digits[j] >= '0' && digits[j] <= '9'; j++ {
		if n = n*10 + int(digits[j]-'0'); n > t.limit {
			break
		}
		held[n] = true
	}
}

// writeAttribute writes one attribute of an opening tag. The value escapes the ampersand, the
// angle brackets, the double quote and every control character below U+0020 as XML character
// references, so no value can end the attribute or the tag.
func writeAttribute(b *strings.Builder, key, value string) {
	b.WriteString(" ")
	b.WriteString(key)
	b.WriteString(`="`)
	for i := range len(value) {
		switch c := value[i]; {
		case c == '&':
			b.WriteString("&amp;")
		case c == '<':
			b.WriteString("&lt;")
		case c == '>':
			b.WriteString("&gt;")
		case c == '"':
			b.WriteString("&quot;")
		case c < 0x20:
			b.WriteString("&#")
			b.WriteString(strconv.Itoa(int(c)))
			b.WriteString(";")
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString(`"`)
}

// utf16Length is the count of UTF-16 units of text, each byte that is not valid UTF-8 one unit
// (U+FFFD).
func utf16Length(text string) int {
	n := 0
	for _, r := range text {
		n += utf16.RuneLen(r)
	}
	return n
}

// formatTime writes t as the texts write a time: RFC 3339 in UTC, with the fraction of a second
// only where t has one.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
