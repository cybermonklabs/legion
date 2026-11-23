// Package wordrecords is INFRA-3231's R8 records of the words people ask agents about. A job of
// the server (Job) reads each text a person writes from the event log, after the write has
// committed, and asks a model whether the person asks what a word of an agent's text means, says
// they cannot understand the text, or complains about its words: the selection prompt
// (select.md). The dataset command (cmd/writing-check dataset classify) gives the dataset's texts
// their verdicts with the same prompt and the same user message (Input), so one definition of a
// question about words serves R1's dataset and R8's records.
package wordrecords

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/prompttags"
)

// SelectPrompt is the selection prompt, the system prompt of every call. dataset/DATASET in
// cmd/writing-check records its SHA-256, which a change to the file breaks.
//
//go:embed select.md
var SelectPrompt string

// The verdicts of the selection prompt.
const (
	WordMeaning      = "asks_word_meaning"
	CannotUnderstand = "cannot_understand"
	JargonComplaint  = "jargon_complaint"
	AgentQuestion    = "asks_agent_question"
	None             = "none"
)

// Verdicts is every verdict, in the prompt's order.
var Verdicts = []string{WordMeaning, CannotUnderstand, JargonComplaint, AgentQuestion, None}

// AboutWords is whether a verdict is one of the three about words: the person says the words of
// the agent's text are the problem.
func AboutWords(verdict string) bool {
	return verdict == WordMeaning || verdict == CannotUnderstand || verdict == JargonComplaint
}

// VerdictSchema is the JSON schema of a reply: one verdict, and the words it is about.
var VerdictSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"verdict": {"type": "string", "enum": ["asks_word_meaning", "cannot_understand", "jargon_complaint", "asks_agent_question", "none"]},
		"words": {"type": "array", "items": {"type": "string"}}
	},
	"required": ["verdict", "words"],
	"additionalProperties": false
}`)

// How much of each part of a text the user message holds, in bytes.
const (
	AgentChars  = 12_000
	PersonChars = 8_000
	QuoteChars  = 2_000
)

// Text is one text of the user message: its kind (comment, message, ask, document, answer,
// resolution, review) and its words.
type Text struct {
	Kind string
	Text string
}

// messageTags are the tags of the user message (Input), escaped in each text it holds
// (prompttags), so no text opens or closes one; ParseVerdict reads the reply's words back.
var messageTags = prompttags.New("agent_text", "quoted_part", "person_text")

// Input is the user message of one call: the agent text the person's text replies to, left out
// where its words are empty; what the person's text points to, left out where it is empty: the
// part of a document its anchor quotes, then the lines it quotes itself (SplitQuote); and the
// person's own lines. A quoted line is not the person's words (INFRA-3231's R7, Quote reply), so
// a question in it is no question of theirs, while the words they ask about may be in it. Each
// part is cut at its bound (Clip), and then holds each opening and closing of the message's own
// tags escaped (messageTags).
func Input(agent Text, quote string, person Text) string {
	own, quoted := SplitQuote(person.Text)
	pointed := quote
	if quoted != "" {
		if pointed != "" {
			pointed += "\n\n"
		}
		pointed += quoted
	}
	var b strings.Builder
	if strings.TrimSpace(agent.Text) != "" {
		fmt.Fprintf(&b, "<agent_text kind=%q>\n%s\n</agent_text>\n\n", agent.Kind, messageTags.Escape(Clip(agent.Text, AgentChars)))
	}
	if pointed != "" {
		fmt.Fprintf(&b, "<quoted_part>\n%s\n</quoted_part>\n\n", messageTags.Escape(Clip(pointed, QuoteChars)))
	}
	fmt.Fprintf(&b, "<person_text kind=%q>\n%s\n</person_text>\n", person.Kind, messageTags.Escape(Clip(own, PersonChars)))
	return b.String()
}

// quoteLine matches a line Markdown reads as a quote's: `>` after at most three spaces, and the
// one space that may follow it.
var quoteLine = regexp.MustCompile(`^ {0,3}> ?`)

// SplitQuote splits a person's text into their own lines and the lines they quote: a line that
// starts with `>` (after at most three spaces), as Quote reply writes one. A quoted line loses
// that `>` and the space after it. Each part keeps its lines in order, without the blank lines at
// its ends. A text that quotes nothing is its own words as written, and quotes "".
func SplitQuote(text string) (own, quoted string) {
	lines := strings.Split(text, "\n")
	if !slices.ContainsFunc(lines, quoteLine.MatchString) {
		return text, ""
	}
	var ownLines, quotedLines []string
	for _, line := range lines {
		if loc := quoteLine.FindStringIndex(line); loc != nil {
			quotedLines = append(quotedLines, line[loc[1]:])
		} else {
			ownLines = append(ownLines, line)
		}
	}
	return trimBlankLines(ownLines), trimBlankLines(quotedLines)
}

// trimBlankLines joins lines, leaving out the blank lines at either end.
func trimBlankLines(lines []string) string {
	blank := func(line string) bool { return strings.TrimSpace(line) == "" }
	for len(lines) > 0 && blank(lines[0]) {
		lines = lines[1:]
	}
	for len(lines) > 0 && blank(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// Clip is s cut to at most n bytes at a rune boundary, with a line that says how much was cut.
func Clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[cut here: %d more characters]", utf8.RuneCountInString(s[cut:]))
}

// ParseVerdict reads a reply of VerdictSchema: its verdict, and its words, an empty list where it
// gives none, each as the text holds it (the message's escape undone, messageTags). A reply that
// is not one verdict of the schema is an error that quotes it.
func ParseVerdict(value json.RawMessage) (verdict string, words []string, err error) {
	var reply struct {
		Verdict string   `json:"verdict"`
		Words   []string `json:"words"`
	}
	if err := json.Unmarshal(value, &reply); err != nil || !slices.Contains(Verdicts, reply.Verdict) {
		return "", nil, fmt.Errorf("the reply is not one verdict of the schema: %.300s", value)
	}
	if reply.Words == nil {
		reply.Words = []string{}
	}
	for i, word := range reply.Words {
		reply.Words[i] = messageTags.Unescape(word)
	}
	return reply.Verdict, reply.Words, nil
}
