package reviewer

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Keep is one rule of what a rewrite keeps (INFRA-3231's R5, INFRA-3552's R1).
type Keep string

// The rules of what a rewrite keeps.
const (
	// KeepMostWords: a third of the text's words or more, for every kind. A rewrite of fewer
	// words is a stub or a summary in place of the text ("placeholder", "(no change)", an empty
	// body), which the reader would read in place of the whole text.
	KeepMostWords Keep = "most_words"
	// KeepNoRecordTags: no tag of the record's markup (recordTags) that the text does not hold,
	// for every kind. A rewrite that holds one pasted the record, or a part of it, into the text
	// ("<issue key=", "<document name="), which the reader would read as the writer's.
	KeepNoRecordTags Keep = "no_record_tags"
	// KeepNoRecordRun: no run of more than 100 words in a row (recordRunWords) that a text of
	// the record holds and neither the text nor the writer's answer holds, for every kind. A
	// rewrite that holds one pasted the record in place of the text (a short comment written
	// again as the spec it points to), which the reader would read as the writer's, and the
	// text's own facts are lost under it.
	KeepNoRecordRun Keep = "no_record_run"
	// KeepFenceLines: each line of the text that starts with ":::", a typed block's fence, byte
	// for byte and as often as the text holds it, for every kind. A rewrite that changes or
	// drops one writes the block as prose, or breaks it.
	KeepFenceLines Keep = "fence_lines"
	// KeepOptionCount: an ask's number of options.
	KeepOptionCount Keep = "option_count"
	// KeepOptionOrder: the order of an ask's options.
	KeepOptionOrder Keep = "option_order"
	// KeepOptionLabels: each option's label, byte for byte. An option's id is the server's, which
	// an edit that keeps the labels and their order keeps (OptionWords), so a rewrite has none.
	KeepOptionLabels Keep = "option_labels"
	// KeepChoice: an ask's single or multiple choice.
	KeepChoice Keep = "choice"
	// KeepFirstLine: an ask's first line, byte for byte, where the question has more than one
	// line and that line has 12 words or fewer. Legion's controller finds a finding's ask by it.
	KeepFirstLine Keep = "first_line"
	// KeepRefLine: each line of an ask that starts with "Ref: ", byte for byte, wherever it
	// stands.
	KeepRefLine Keep = "ref_line"
	// KeepPictureLines: each line of an ask that holds only a picture, byte for byte.
	KeepPictureLines Keep = "picture_lines"
	// KeepLinks: each link of an ask's question, somewhere in the rewrite's question, and each link
	// of an option's description, in the rewrite's description of that option.
	KeepLinks Keep = "links"
	// KeepOptionLines: each option line of a decision block that the ask-edit route does not
	// rewrite, because it holds more than its plain text (docs.LostOptionWords).
	KeepOptionLines Keep = "option_lines"
	// KeepCommentFirstLine: a comment's first line, byte for byte, where the comment has more
	// than one line and that line has 12 words or fewer.
	KeepCommentFirstLine Keep = "comment_first_line"
)

// firstLineWords is the most words a first line a rewrite keeps has.
const firstLineWords = 12

// recordRunWords is the most words in a row a rewrite may hold of the record that neither the
// text nor the writer's answer holds. On INFRA-3231's harness runs, each rewrite that pasted the
// record held a run of several hundred words of it; each other rewrite with a run past 100
// words lost facts of the text, and the longest run of a rewrite that lost none was 67 words.
const recordRunWords = 100

// NotApplied is a rewrite that breaks a rule of what a rewrite keeps, which is therefore not
// applied: the rule and the reason, which the card shows as the failed rewrite's tooltip.
type NotApplied struct {
	Rule   Keep
	Reason string
}

func (n *NotApplied) Error() string {
	return "the rewrite is not applied: " + n.Reason
}

// Preserve weighs rewrite against the text of record by the rules of what a rewrite keeps, in
// the order of Keep's constants, and answers the first rule it breaks; nil where it breaks none.
// answer is the writer's answer the rewrite read, "" for none. Every kind keeps a third of its
// words or more and each of its typed blocks' fences, holds no tag of the record that the text
// does not hold, and holds no long run of the record's words that neither the text nor answer
// holds. An ask and a decision block keep their options, their choice, their first line, their
// `Ref: ` line, their picture lines, the links of their question and the links of each option's
// description; a decision block also keeps the option lines its document's ask-edit route does
// not rewrite, which Preserve weighs where the record holds the document and the block's id; a
// comment keeps its first line. Every other kind keeps nothing more.
func Preserve(record Record, answer string, rewrite Text) *NotApplied {
	original := record.Text
	if broken := keepMostWords(original, rewrite); broken != nil {
		return broken
	}
	if broken := keepNoRecordTags(original, rewrite); broken != nil {
		return broken
	}
	if broken := keepNoRecordRun(record, answer, rewrite); broken != nil {
		return broken
	}
	if broken := keepFenceLines(original.Body, rewrite.Body); broken != nil {
		return broken
	}
	switch {
	case original.Kind.asks():
		if broken := keepOptions(original, rewrite); broken != nil {
			return broken
		}
		if broken := keepLines(original.Body, rewrite.Body, KeepFirstLine); broken != nil {
			return broken
		}
		if broken := keepOptionLinks(original.Options, rewrite.Options); broken != nil {
			return broken
		}
		if original.Kind == KindDecisionBlock {
			return keepOptionLines(record, rewrite)
		}
	case original.Kind == KindComment:
		return keepFirstLine(original.Body, rewrite.Body, KeepCommentFirstLine)
	}
	return nil
}

// keepMostWords weighs a rewrite's words against the text's: the rewrite has a third of them or
// more, counted over every field a reader sees (the title, the body and each option).
func keepMostWords(original, rewrite Text) *NotApplied {
	was, now := wordCount(original), wordCount(rewrite)
	if 3*now >= was {
		return nil
	}
	words := fmt.Sprintf("%d words", now)
	if now == 1 {
		words = "1 word"
	}
	return &NotApplied{KeepMostWords, fmt.Sprintf("the rewrite has %s, fewer than a third of the text's %d", words, was)}
}

// wordCount is the number of words of text's title, body and options, words being the runs of
// characters between white space.
func wordCount(text Text) int {
	n := len(strings.Fields(text.Title)) + len(strings.Fields(text.Body))
	for _, option := range text.Options {
		n += len(strings.Fields(option.Label)) + len(strings.Fields(option.Description))
	}
	return n
}

// readerFields is every field of text a reader sees, the title, the body and each option's label
// and description, one after the other on lines of their own.
func readerFields(text Text) string {
	var b strings.Builder
	b.WriteString(text.Title + "\n" + text.Body)
	for _, option := range text.Options {
		b.WriteString("\n" + option.Label + "\n" + option.Description)
	}
	return b.String()
}

// keepNoRecordTags weighs a rewrite's fields against the text's: the rewrite holds no tag of the
// record's markup, opening or closing, that the text does not hold, a tag found as the message's
// escape finds one (messageTags) and named in any case. A text that holds such a tag as its own
// words, an HTML title for one, keeps it in its rewrite.
func keepNoRecordTags(original, rewrite Text) *NotApplied {
	held := messageTags.Found(readerFields(original))
	for _, tag := range messageTags.Found(readerFields(rewrite)) {
		if !slices.ContainsFunc(held, func(own string) bool { return strings.EqualFold(own, tag) }) {
			return &NotApplied{KeepNoRecordTags, fmt.Sprintf("the rewrite holds %s, a tag of the record, which the text does not hold", strconv.Quote(tag+">"))}
		}
	}
	return nil
}

// keepNoRecordRun weighs a rewrite's fields against the record: the rewrite holds no run of more
// than recordRunWords words, in a row and in their order, that a text of the record holds
// (Record.parts) and that neither the text nor answer holds. Words are compared as runWords reads
// them, so a run pasted with its marks changed is still the record's.
func keepNoRecordRun(record Record, answer string, rewrite Text) *NotApplied {
	n := recordRunWords + 1
	words := runWords(readerFields(rewrite))
	if len(words) < n {
		return nil
	}
	held := newRunIndex(n)
	for _, part := range record.parts() {
		held.add(runWords(part))
	}
	if len(held.at) == 0 {
		return nil
	}
	own := newRunIndex(n)
	own.add(runWords(readerFields(record.Text)))
	own.add(runWords(answer))
	first, last := -1, -1
	runHashes(words, n, func(i int, h uint64) {
		run := words[i : i+n]
		pasted := held.holds(run, h) && !own.holds(run, h)
		switch {
		case pasted && first < 0:
			first, last = i, i
		case pasted && last == i-1:
			last = i
		}
	})
	if first < 0 {
		return nil
	}
	whose := "the text does not hold"
	if answer != "" {
		whose = "neither the text nor the writer's answer holds"
	}
	length := last - first + n
	return &NotApplied{KeepNoRecordRun, fmt.Sprintf("the rewrite holds %d words in a row of the record that %s, from %s",
		length, whose, strconv.Quote(strings.Join(words[first:first+runShownWords], " ")+" …"))}
}

// runShownWords is how many words of a pasted run the reason shows.
const runShownWords = 12

// parts are the texts of the record a call's user message shows besides the text itself
// (Record.input): each text of the thread and of the writer's, the issue's title, the project's
// name, the document unless it is the text itself, and the decisions document.
func (r Record) parts() []string {
	parts := []string{r.IssueTitle, r.Project, r.Decisions}
	if d := r.Document; d != nil && !(d.Holds && r.Text.Kind == KindDocument) {
		parts = append(parts, d.Markdown)
	}
	for _, entry := range slices.Concat(r.Thread, r.WriterTexts) {
		parts = append(parts, entry.Text)
	}
	return parts
}

// runWords is text's words as keepNoRecordRun compares them: the runs of characters between white
// space, each lower-cased and cut of the marks at its ends that are neither letters nor digits,
// and none left empty.
func runWords(text string) []string {
	words := strings.Fields(text)
	kept := words[:0]
	for _, word := range words {
		word = strings.TrimFunc(word, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		if word != "" {
			kept = append(kept, strings.ToLower(word))
		}
	}
	return kept
}

// runIndex holds each run of n words of the texts added to it, by its hash (runHashes), with the
// words of the run first added under that hash, so a hash that two runs share is told apart.
type runIndex struct {
	n  int
	at map[uint64][]string
}

func newRunIndex(n int) *runIndex { return &runIndex{n: n, at: map[uint64][]string{}} }

// add adds each run of n words of words.
func (x *runIndex) add(words []string) {
	runHashes(words, x.n, func(i int, h uint64) {
		if _, ok := x.at[h]; !ok {
			x.at[h] = words[i : i+x.n]
		}
	})
}

// holds is whether the index holds run, whose hash is h.
func (x *runIndex) holds(run []string, h uint64) bool {
	held, ok := x.at[h]
	return ok && slices.Equal(held, run)
}

// The constants of runHashes: FNV-1a's 64-bit offset basis and prime, the prime also the base of
// the rolling hash, being odd.
const (
	fnvBasis = 14695981039346656037
	fnvPrime = 1099511628211
)

// runHashes calls each with the start and the hash of each run of n words of words, in order:
// each word's FNV-1a hash, rolled into one hash of the run, so each run costs one step past the
// first.
func runHashes(words []string, n int, each func(i int, h uint64)) {
	if len(words) < n {
		return
	}
	hashes := make([]uint64, len(words))
	for i, word := range words {
		h := uint64(fnvBasis)
		for j := range len(word) {
			h = (h ^ uint64(word[j])) * fnvPrime
		}
		hashes[i] = h
	}
	var h, top uint64 = 0, 1
	for i := range n {
		h = h*fnvPrime + hashes[i]
		if i > 0 {
			top *= fnvPrime
		}
	}
	each(0, h)
	for i := n; i < len(words); i++ {
		h = (h-hashes[i-n]*top)*fnvPrime + hashes[i]
		each(i-n+1, h)
	}
}

// fence starts each line of a typed block's fence: its opening (":::check{#c1}") and its close.
const fence = ":::"

// keepFenceLines weighs a rewrite's body against the text's: it holds each line of the text that
// starts with a fence, as many times as the text holds it.
func keepFenceLines(was, now string) *NotApplied {
	held := map[string]int{}
	for _, line := range lines(now) {
		if strings.HasPrefix(line, fence) {
			held[line]++
		}
	}
	for _, line := range lines(was) {
		if !strings.HasPrefix(line, fence) {
			continue
		}
		if held[line] == 0 {
			return &NotApplied{KeepFenceLines, "the rewrite drops or changes the line " + strconv.Quote(line)}
		}
		held[line]--
	}
	return nil
}

// keepOptions weighs a rewrite's options and choice against the ask's: their number, their
// order and each label. Their ids are the server's (OptionWords), so neither side's are weighed.
func keepOptions(original, rewrite Text) *NotApplied {
	was, now := original.Options, rewrite.Options
	if len(now) != len(was) {
		return &NotApplied{KeepOptionCount, fmt.Sprintf("the rewrite has %s, and the ask has %d", optionCount(len(now)), len(was))}
	}
	if !slices.EqualFunc(was, now, sameLabel) && sameLabels(was, now) {
		return &NotApplied{KeepOptionOrder, fmt.Sprintf("the rewrite puts the options in the order %s, and the ask has them in the order %s", labels(now), labels(was))}
	}
	for i := range was {
		if now[i].Label != was[i].Label {
			return &NotApplied{KeepOptionLabels, fmt.Sprintf("option %d has the label %s in the rewrite, and %s in the ask", i+1, strconv.Quote(now[i].Label), strconv.Quote(was[i].Label))}
		}
	}
	if rewrite.Multiple != original.Multiple {
		choice := map[bool]string{false: "single-choice", true: "multiple-choice"}
		return &NotApplied{KeepChoice, fmt.Sprintf("the rewrite makes the %s ask a %s ask", choice[original.Multiple], choice[rewrite.Multiple])}
	}
	return nil
}

// sameLabel reports whether two options have the same label.
func sameLabel(a, b model.AskOption) bool { return a.Label == b.Label }

// sameLabels reports whether two lists hold the same labels, in any order.
func sameLabels(a, b []model.AskOption) bool {
	left, right := make([]string, len(a)), make([]string, len(b))
	for i := range a {
		left[i] = a[i].Label
	}
	for i := range b {
		right[i] = b[i].Label
	}
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}

// labels is each option's label, quoted, joined.
func labels(options []model.AskOption) string {
	quoted := make([]string, len(options))
	for i, option := range options {
		quoted[i] = strconv.Quote(option.Label)
	}
	return strings.Join(quoted, ", ")
}

// optionCount is n options, as a sentence says it.
func optionCount(n int) string {
	if n == 1 {
		return "1 option"
	}
	return fmt.Sprintf("%d options", n)
}

// keepLines weighs a rewrite's question against the ask's: its first line (under the rule
// first), each `Ref: ` line, each picture line and each link.
func keepLines(was, now string, first Keep) *NotApplied {
	if broken := keepFirstLine(was, now, first); broken != nil {
		return broken
	}
	held := lines(now)
	for _, line := range lines(was) {
		if strings.HasPrefix(line, "Ref: ") && !slices.Contains(held, line) {
			return &NotApplied{KeepRefLine, "the rewrite drops or changes the line " + strconv.Quote(line)}
		}
	}
	for _, line := range lines(was) {
		if pictureLine.MatchString(line) && !slices.Contains(held, line) {
			return &NotApplied{KeepPictureLines, "the rewrite drops or changes the picture line " + strconv.Quote(line)}
		}
	}
	for _, link := range links(was) {
		if !strings.Contains(now, link) {
			return &NotApplied{KeepLinks, "the rewrite drops the link " + strconv.Quote(link)}
		}
	}
	return nil
}

// keepOptionLinks weighs a rewrite's options against the ask's: each link of an option's
// description is in the rewrite's description of that option. The options are weighed by their
// place, which keepOptions holds to the ask's with their labels.
func keepOptionLinks(was, now []model.AskOption) *NotApplied {
	for i, option := range was {
		for _, link := range links(option.Description) {
			if !strings.Contains(now[i].Description, link) {
				return &NotApplied{KeepLinks, fmt.Sprintf("the rewrite drops the link %s of option %d's description", strconv.Quote(link), i+1)}
			}
		}
	}
	return nil
}

// keepFirstLine weighs a rewrite's first line against the text's, which it keeps where the text
// has more than one line and its first has firstLineWords words or fewer.
func keepFirstLine(was, now string, rule Keep) *NotApplied {
	original := lines(was)
	if len(original) < 2 || len(strings.Fields(original[0])) > firstLineWords {
		return nil
	}
	if rewritten := lines(now); len(rewritten) == 0 || rewritten[0] != original[0] {
		return &NotApplied{rule, fmt.Sprintf("the rewrite changes the first line %s, which has %d words or fewer", strconv.Quote(original[0]), firstLineWords)}
	}
	return nil
}

// keepOptionLines weighs a decision block's rewritten options by its document's ask-edit route
// (docs.LostOptionWords). A record without the block's document weighs nothing here; the write
// of the rewrite weighs it again.
func keepOptionLines(record Record, rewrite Text) *NotApplied {
	document := record.Document
	if document == nil || !document.Holds || document.BlockID == "" {
		return nil
	}
	err := docs.LostOptionWords(document.Markdown, document.BlockID, rewrite.Options)
	if err == nil {
		return nil
	}
	var refused *docs.ErrAskBlockUnrepresentable
	if errors.As(err, &refused) && refused.Field == "options" {
		return &NotApplied{KeepOptionLines, refused.Reason}
	}
	return &NotApplied{KeepOptionLines, "the decision block cannot be read in its document: " + err.Error()}
}

// lines is text's lines, a final line break ending the last rather than starting another.
func lines(text string) []string {
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// recordTags are the names of the tags a call's user message wraps the record and its other
// inputs in (Record.input, textPart, entryPart, findingsPart).
var recordTags = []string{
	"text", "question", "options", "title", "spec", "thread", "entry", "issue", "project", "document",
	"decisions", "writer_texts", "reader_words", "structure_findings", "detector_findings",
	"question_to_writer", "writer_answer", "words",
}

var (
	// pictureLine is a line that holds only a picture, or pictures: Markdown images and the
	// space around them.
	pictureLine = regexp.MustCompile(`^\s*(?:!\[[^\]]*\]\([^)]*\)\s*)+$`)
	// linkTarget is a Markdown link's or picture's address, and bareLink an address written in
	// the text as it is.
	linkTarget = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)`)
	bareLink   = regexp.MustCompile(`(?:https?|dispatch)://[^\s<>()\[\]"'` + "`" + `]+`)
)

// links is each link of text once: each Markdown link's address and each address written bare,
// without the punctuation that ends a sentence after it.
func links(text string) []string {
	var found []string
	add := func(link string) {
		if link = strings.TrimRight(link, ".,;:!?"); link != "" && !slices.Contains(found, link) {
			found = append(found, link)
		}
	}
	for _, match := range linkTarget.FindAllStringSubmatch(text, -1) {
		add(match[1])
	}
	for _, match := range bareLink.FindAllString(text, -1) {
		add(match)
	}
	return found
}
