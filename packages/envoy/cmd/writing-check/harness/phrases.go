package harness

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/cmd/writing-check/harness/llm"
)

// The phrases judgement follows the measured definition of a vacuous verb phrase (INFRA-3231's
// vacuous-verb definition, the rule of writing-check's labeller): it judges sentences one at a
// time. The candidates are the sentences of the text that hold a verb the definition names (and
// the phrases skills/plain-words tells agents to stop); the grader judges each with the sentences
// next to it. A text with no candidate has no finding and no call.

// candidateVerb matches the forms of the verbs the definition names.
var candidateVerb = regexp.MustCompile(`(?i)\b(names?|named|naming|shows?|showed|shown|showing|pays?|paid|paying|carr(?:y|ies|ied|ying)|holds?|held|holding|lands?|landed|landing|arrives?|arrived|arriving|unblocks?|unblocked|surfaces?|surfaced)\b`)

// sentenceEnd is the end of a sentence inside a line: a stop, then space and the start of the
// next sentence.
var sentenceEnd = regexp.MustCompile("[.!?][\"')\\]]?\\s+[A-Z0-9\"'(\\[`*]")

// fencedBlock is a Markdown code block.
var fencedBlock = regexp.MustCompile("(?s)```.*?```")

// listMarker is a list item's marker or a heading's at the start of a line.
var listMarker = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)]|#+)\s+`)

// sentencesOf splits text into sentences: no code block, no quotation line; each table cell,
// heading and list item a sentence of its own; a line split after each stop.
func sentencesOf(text string) []string {
	var out []string
	// split adds the sentences of one line or one table cell.
	split := func(s string) {
		for {
			loc := sentenceEnd.FindStringIndex(s)
			if loc == nil {
				break
			}
			cut := loc[1] - 1
			out = append(out, strings.TrimSpace(s[:cut]))
			s = s[cut:]
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	for _, line := range strings.Split(fencedBlock.ReplaceAllString(text, "\n"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "", strings.HasPrefix(line, ">"), strings.HasPrefix(line, ":::"):
		case strings.HasPrefix(line, "|"):
			for _, cell := range strings.Split(line, "|") {
				if cell = strings.TrimSpace(cell); strings.Trim(cell, "-: ") != "" {
					split(cell)
				}
			}
		default:
			split(listMarker.ReplaceAllString(line, ""))
		}
	}
	return out
}

// phraseCandidate is one candidate sentence, with the sentences before and after it.
type phraseCandidate struct {
	ID       string `json:"id"`
	Before   string `json:"before,omitempty"`
	Sentence string `json:"sentence"`
	After    string `json:"after,omitempty"`
}

// phraseCandidates are the sentences of text that hold a candidate verb outside inline code,
// each with the two sentences before it and the one after.
func phraseCandidates(text string) []phraseCandidate {
	sentences := sentencesOf(text)
	var out []phraseCandidate
	for i, s := range sentences {
		if !candidateVerb.MatchString(inlineCode.ReplaceAllString(s, " CODE ")) {
			continue
		}
		c := phraseCandidate{ID: "s" + strconv.Itoa(len(out)+1), Sentence: s}
		c.Before = strings.Join(sentences[max(0, i-2):i], " ")
		if i+1 < len(sentences) {
			c.After = sentences[i+1]
		}
		out = append(out, c)
	}
	return out
}

// phraseChunk bounds the candidates one call judges.
const phraseChunk = 30

// gradeSentences gives the phrases judgement sentence by sentence: the candidates of in.Text,
// each judged by the spec's prompt, which base holds with the reply's schema; the sentences the
// grader finds vacuous are the findings, each quoted whole. The calls of the chunks are summed
// into one Call.
// A sentence judgement reads only its candidates, so its input mode is always trimmed.
func (g *Grader) gradeSentences(ctx context.Context, spec judgementSpec, in GradeInput, base llm.Request) Grade {
	grade := Grade{Inputs: InputsTrimmed}
	cands := phraseCandidates(in.Text)
	grade.Candidates = len(cands)
	if len(cands) == 0 {
		return grade
	}
	var total Call
	var replies []json.RawMessage
	for start := 0; start < len(cands); start += phraseChunk {
		chunk := cands[start:min(start+phraseChunk, len(cands))]
		list, _ := json.MarshalIndent(chunk, "", " ")
		req := base
		req.User = tag("candidates", string(list))
		resp, refused, err := g.call(ctx, gradeRequest{req: req})
		grade.Refused = append(grade.Refused, refused...)
		total = addCall(total, callOf(resp))
		if err != nil {
			grade.Call, grade.Error = &total, err.Error()
			return grade
		}
		replies = append(replies, resp.Value)
		var reply struct {
			Candidates []map[string]string `json:"candidates"`
		}
		if err := json.Unmarshal(resp.Value, &reply); err != nil {
			grade.Call, grade.Error = &total, "the phrases reply: "+err.Error()
			return grade
		}
		verdicts := map[string]map[string]string{}
		for _, r := range reply.Candidates {
			verdicts[r["1_id"]] = r
		}
		for _, c := range chunk {
			r := verdicts[c.ID]
			switch r["4_verdict"] {
			case spec.verdicts[0]:
				grade.Items = append(grade.Items, Item{Quote: c.Sentence, Note: r["3_rewrite"]})
			case "unresolved":
				grade.Unresolved++
			default:
				grade.Dropped++
			}
		}
	}
	grade.Count, grade.Call = len(grade.Items), &total
	grade.Reply, _ = json.Marshal(replies)
	return grade
}

// addCall is the sum of two calls' tokens, cost, attempts and time, with b's model.
func addCall(a, b Call) Call {
	b.Input += a.Input
	b.Output += a.Output
	b.CacheWrite += a.CacheWrite
	b.CacheRead += a.CacheRead
	b.CostUSD += a.CostUSD
	b.Attempts += a.Attempts
	b.Seconds += a.Seconds
	return b
}

// phraseSchema is the reply of a phrases call: for each candidate, its id, the reason, the
// rewrite and a verdict, one of verdicts.
func phraseSchema(verdicts []string) json.RawMessage {
	return schemaJSON(object(property{"candidates", arrayOf(object(
		property{"1_id", stringSchema}, property{"2_reason", stringSchema}, property{"3_rewrite", stringSchema},
		property{"4_verdict", enum(verdicts...)},
	))}))
}
