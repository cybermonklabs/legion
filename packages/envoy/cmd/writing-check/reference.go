package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
)

// The strata of the measurement: real writes, and planted copies of real writes.
const (
	stratumReal    = "real"
	stratumPlanted = "planted"
)

// writeNotApplicable is the reference label of a write that holds no prose sentence.
const writeNotApplicable = "not_applicable"

// The shapes recall is reported over: the three the owner named. A vacuous sentence of shape
// "other" has a main verb in no list (no name, show or pay): the no-list set.
var namedShapes = []string{shapeNames, shapeShows, shapePays}

// refSentence is one prose sentence of a labelled write, with its reference verdict and shape.
// A planted copy's planted sentence takes them from plant's manifest (a fault is vacuous, a
// control not vacuous, each of the planted shape); every other sentence takes the labeller's.
type refSentence struct {
	ID      string
	Field   string
	Kind    string
	Text    string
	Verdict string
	Shape   string
	// Planted is the planted kind (fault or control) of the planted sentence, else "".
	Planted string
	// Verb is a planted sentence's verb.
	Verb string
}

// refWrite is one labelled write: its key, stratum, reference label and sentences. Label is the
// write's reference label: failure when it holds a reference-vacuous sentence, unresolved when it
// holds none and an unresolved one, pass, not_applicable for a write with no prose sentence, and
// error where the labeller failed or a planted sentence could not be found once.
type refWrite struct {
	Key       writeKey
	Family    string
	Stratum   string
	Label     string
	Sentences []refSentence
}

// id is the write's id in every file: event id / source id [/ copy].
func (w refWrite) id() string { return w.Key.String() }

// usable is whether the write has a reference label the rates read.
func (w refWrite) usable() bool { return w.Label != writeError && w.Label != writeNotApplicable }

// proseSentences is the write's sentences as the code stage reads them. The labels hold each
// sentence's field and text in document order, which is all candidates.Find reads (the text and,
// for the context, the field), so the candidates are the ones the server's check finds.
func (w refWrite) proseSentences() []prose.Sentence {
	out := make([]prose.Sentence, len(w.Sentences))
	for i, s := range w.Sentences {
		out[i] = prose.Sentence{Field: s.Field, Kind: s.Kind, Text: s.Text}
	}
	return out
}

// loadReferences reads a labels file and, where it labels planted copies, plant's manifest, and
// answers each labelled write once (the last line of a write wins, as label --resume appends),
// in the order each write first appears. A planted copy needs the manifest to name it.
func loadReferences(labelsPath, plantedPath string) ([]refWrite, error) {
	planted := map[string]plantedCase{}
	if plantedPath != "" {
		rows, err := readPlanted(plantedPath)
		if err != nil {
			return nil, err
		}
		for _, p := range rows {
			planted[p.Copy] = p
		}
	}
	var writes []refWrite
	at := map[writeKey]int{}
	err := eachLine(labelsPath, func(line []byte, _ int) error {
		var r labelRecord
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		w, err := referenceOf(r, planted)
		if err != nil {
			return err
		}
		if i, ok := at[w.Key]; ok {
			writes[i] = w
			return nil
		}
		at[w.Key] = len(writes)
		writes = append(writes, w)
		return nil
	})
	if err == nil && len(writes) == 0 {
		err = errors.New("the labels file holds no write")
	}
	return writes, err
}

// referenceOf is one labels line as a reference write.
func referenceOf(r labelRecord, planted map[string]plantedCase) (refWrite, error) {
	w := refWrite{Key: r.key(), Family: r.Family, Stratum: stratumReal}
	for _, s := range r.Sentences {
		w.Sentences = append(w.Sentences, refSentence{ID: s.ID, Field: s.Field, Kind: s.Kind, Text: s.Text, Verdict: s.Verdict, Shape: s.Shape})
	}
	if r.Copy != "" {
		p, ok := planted[r.Copy]
		if !ok {
			return refWrite{}, fmt.Errorf("planted copy %s is in no planted manifest given (--planted)", r.Copy)
		}
		if p.EventID != r.EventID || p.SourceID != r.SourceID {
			return refWrite{}, fmt.Errorf("planted copy %s copies %d/%s in the manifest, not %d/%s", r.Copy, p.EventID, p.SourceID, r.EventID, r.SourceID)
		}
		w.Stratum = stratumPlanted
		found := -1
		for i, s := range w.Sentences {
			if s.Field == p.Field && s.Text == p.SentenceProse {
				if found >= 0 {
					found = -2
					break
				}
				found = i
			}
		}
		if found < 0 {
			w.Label = writeError
			return w, nil
		}
		s := &w.Sentences[found]
		s.Planted, s.Verb, s.Shape, s.Verdict = p.Kind, p.Verb, p.Shape, expectedVerdict(p.Kind)
	}
	switch {
	case r.Error != "" || r.WriteLabel == writeError:
		w.Label = writeError
	case len(w.Sentences) == 0:
		w.Label = writeNotApplicable
	default:
		w.Label = writePass
		for _, s := range w.Sentences {
			if s.Verdict == verdictVacuous {
				w.Label = writeFailure
				break
			}
			if s.Verdict == verdictUnresolved {
				w.Label = writeUnresolved
			}
		}
	}
	return w, nil
}

// inStratum is the writes of one stratum.
func inStratum(writes []refWrite, stratum string) []refWrite {
	var out []refWrite
	for _, w := range writes {
		if w.Stratum == stratum {
			out = append(out, w)
		}
	}
	return out
}

// heldout is how the no-list sentences are split into a tuning half and a held-out half: by the
// seed, a unit goes to the held-out half when the hash of the seed and the unit's key falls under
// share. The unit of a planted fault is its verb, so no verb of the held-out half is ever tuned
// on; the unit of a labelled sentence is its write. A nil heldout splits nothing.
type heldout struct {
	Seed  int64   `json:"seed"`
	Share float64 `json:"share"`
}

// unit is the key the split draws a no-list sentence by.
func splitUnit(w refWrite, s refSentence) string {
	if s.Planted != "" && s.Verb != "" {
		return "verb:" + s.Verb
	}
	return "write:" + w.id()
}

// held is whether the unit falls in the held-out half.
func (h *heldout) held(unit string) bool {
	sum := sha256.Sum256([]byte(strconv.FormatInt(h.Seed, 10) + "\x00" + unit))
	return float64(binary.BigEndian.Uint64(sum[:8]))/math.Exp2(64) < h.Share
}

// isNoList is whether a reference-vacuous sentence's main verb is in no list the bundle carries,
// which the labeller and plant both record as the shape "other".
func isNoList(s refSentence) bool { return s.Shape == shapeOther }

// recallSentence is whether a sentence is in the recall population: every reference-vacuous
// sentence of a usable write.
func recallSentence(w refWrite, s refSentence) bool {
	return w.usable() && s.Verdict == verdictVacuous
}

// candidateSentences maps each candidate's sentence to its index among sentences. Find answers
// its candidates strongest first, not in document order, so each candidate takes the first
// sentence not yet taken that has its field and text.
func candidateSentences(sentences []prose.Sentence, cands []candidateRef) ([]int, error) {
	type key struct{ field, text string }
	free := map[key][]int{}
	for i, s := range sentences {
		k := key{s.Field, s.Text}
		free[k] = append(free[k], i)
	}
	out := make([]int, len(cands))
	for n, c := range cands {
		k := key{c.Field, c.Text}
		idx := free[k]
		if len(idx) == 0 {
			return nil, fmt.Errorf("candidate %s (%q) is no sentence of the write", c.ID, c.Text)
		}
		out[n], free[k] = idx[0], idx[1:]
	}
	return out, nil
}

// candidateRef is a candidate as the outputs store it.
type candidateRef struct {
	ID         string `json:"id"`
	SentenceID string `json:"sentence_id"`
	Field      string `json:"field"`
	Text       string `json:"text"`
	Reason     string `json:"reason"`
}
