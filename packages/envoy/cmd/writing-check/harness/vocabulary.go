package harness

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Vocabulary is the words the person the dataset is about wrote himself, each with the time of
// the first text of his that holds it: the owner-texts.jsonl.gz that `writing-check dataset
// owner-texts` writes. The undefined judgement reads, for each case, the words of the case's
// text he wrote before it (score --vocabulary).
type Vocabulary struct {
	first map[string]time.Time
}

// ReadVocabulary reads the owner's texts at path, one JSON line each with its "time" and its
// "text". It refuses a text whose time it cannot read, since no word of such a text can be
// placed before or after a case.
func ReadVocabulary(path string) (*Vocabulary, error) {
	v := &Vocabulary{first: map[string]time.Time{}}
	name := filepath.Base(path)
	err := eachGzipLine(path, func(line []byte) error {
		var t struct {
			Time string `json:"time"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(line, &t); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		at, err := time.Parse(time.RFC3339Nano, t.Time)
		if err != nil {
			return fmt.Errorf("%s: the time of a text: %w", name, err)
		}
		for _, w := range vocabularyWords(t.Text) {
			if first, ok := v.first[w]; !ok || at.Before(first) {
				v.first[w] = at
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(v.first) == 0 {
		return nil, fmt.Errorf("%s holds no word", path)
	}
	return v, nil
}

// vocabularyWords is text's words as a vocabulary matches them: its tokens (lowercase runs of
// letters, digits and '-', '_' and '.' inside a word), each of two characters or more that
// holds a letter. So "Train's" and "train" are one word, and a number is none.
func vocabularyWords(text string) []string {
	words := tokens(text)
	out := words[:0]
	for _, w := range words {
		if utf8.RuneCountInString(w) >= 2 && strings.ContainsFunc(w, unicode.IsLetter) {
			out = append(out, w)
		}
	}
	return out
}

// Known is the words of text that the person wrote himself in a text dated before at, each
// once, in the order text first uses them. A word he first wrote at that time or after it is
// not known: his later question about a word does not count as knowing it.
func (v *Vocabulary) Known(text string, at time.Time) []string {
	var known []string
	seen := map[string]bool{}
	for _, w := range vocabularyWords(text) {
		if first, ok := v.first[w]; ok && first.Before(at) && !seen[w] {
			seen[w] = true
			known = append(known, w)
		}
	}
	return known
}

// knownBefore is the words of text, what is read of case c, that the person wrote himself before
// the case, at the time the undefined judgement cuts at (Known at c.writtenAt): none for a nil
// vocabulary, and none for a case with no time, as none can be placed before it.
func (v *Vocabulary) knownBefore(c *Case, text string) []string {
	if v == nil {
		return nil
	}
	at, ok := c.writtenAt()
	if !ok {
		return nil
	}
	return v.Known(text, at)
}

// writtenAt is the time the agent wrote the case's text: its write_time, else the time of the
// text itself. It is false where the case holds neither as a time.
func (c *Case) writtenAt() (time.Time, bool) {
	for _, s := range []string{c.WriteTime, c.Agent.Time} {
		if at, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return at, true
		}
	}
	return time.Time{}, false
}
