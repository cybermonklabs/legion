package main

import (
	"bytes"
	"context"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
)

func TestEveryPlantedSentenceReadsAsOneSentence(t *testing.T) {
	sentences := map[string]bool{}
	for _, v := range plantVerbs {
		for _, kind := range []string{kindFault, kindControl} {
			for _, s := range v.subjects(kind) {
				for _, tense := range tenses {
					for _, rest := range v.rests(kind) {
						text := v.sentence(s, tense, rest)
						if read := prose.ProseSentences("body", text); len(read) != 1 {
							t.Errorf("%q reads as %d sentences", text, len(read))
						}
						if sentences[text] {
							t.Errorf("%q is made twice", text)
						}
						sentences[text] = true
					}
				}
			}
		}
	}
	shapes := map[string]int{}
	for _, v := range plantVerbs {
		shapes[v.shape]++
		if n := len(v.subjects(kindFault)) * len(v.rests(kindFault)) * len(tenses); v.shape != shapeOther && n < 40 {
			t.Errorf("%s makes %d faults, fewer than 40", v.forms.base, n)
		}
		if n := len(v.subjects(kindControl)) * len(v.rests(kindControl)) * len(tenses); v.shape != shapeOther && n < 40 {
			t.Errorf("%s makes %d controls, fewer than 40", v.forms.base, n)
		}
		for _, form := range []string{v.forms.base, v.forms.third, v.forms.past} {
			if v.shape == shapeOther && slices.Contains(strings.Fields("name names named show shows showed pay pays paid"), form) {
				t.Errorf("the other verb %q is a named shape", form)
			}
		}
	}
	if shapes[shapeNames] != 1 || shapes[shapeShows] != 1 || shapes[shapePays] != 1 || shapes[shapeOther] < 10 {
		t.Errorf("verbs by shape = %v", shapes)
	}
}

// literalSubjects are nouns of things that literally display, carry, hold or pay. No fault's
// subject may be one, so no fault has a literal reading.
var literalSubjects = strings.Fields("plan log page dashboard output view table report screen wallet invoice queue pipe bucket record field list diff patch summary")

func TestFaultsHaveNoLiteralReadingAndStateNoValue(t *testing.T) {
	for _, v := range plantVerbs {
		for _, s := range v.subjects(kindFault) {
			for _, w := range strings.Fields(strings.ToLower(s.text)) {
				if slices.Contains(literalSubjects, strings.TrimSuffix(w, "s")) || slices.Contains(literalSubjects, w) {
					t.Errorf("%s fault subject %q is a thing that literally displays, carries, holds or pays", v.forms.base, s.text)
				}
			}
		}
		for _, rest := range v.rests(kindFault) {
			if strings.ContainsAny(rest, "0123456789`$\"'") {
				t.Errorf("%s fault object %q states a value", v.forms.base, rest)
			}
			if v.forms.base != "pay" && strings.Contains(rest, "change") {
				t.Errorf("%s fault object %q can repeat its subject (\"This change … the change\")", v.forms.base, rest)
			}
		}
	}
}

func TestPlantPutsOneSentenceInEachCopyAndPairsEachFaultWithAControl(t *testing.T) {
	dir := t.TempDir()
	cases := syntheticCorpus(map[string]int{"comment": 20, "document": 20, "message": 20, "result": 20, "issue_title": 20})
	corpus := writeCases(t, dir, "corpus.jsonl", cases)
	manifestIn := writeCases(t, dir, "set.jsonl", cases)
	out := filepath.Join(dir, "planted")
	var stdout, stderr bytes.Buffer
	args := []string{"plant", "--in", manifestIn, "--corpus", corpus, "--seed", "11", "--per-shape", "6", "--nolist", "8", "--controls", "--out", out}
	if code := run(context.Background(), args, &stdout, &stderr); code != 0 {
		t.Fatalf("plant exit %d: %s", code, stderr.String())
	}
	manifest := readJSONL[plantedCase](t, filepath.Join(out, "manifest.jsonl"))
	planted := readJSONL[Case](t, filepath.Join(out, "planted.jsonl"))
	if len(manifest) != 2*(3*6+8) {
		t.Fatalf("%d planted cases, want %d", len(manifest), 2*(3*6+8))
	}
	original := map[writeKey][]Case{}
	for _, c := range cases {
		k := writeKey{EventID: c.EventID, SourceID: c.SourceID}
		original[k] = append(original[k], c)
	}
	copies := map[string][]Case{}
	for _, c := range planted {
		if c.Tag != plantedTag || c.Copy == "" {
			t.Fatalf("planted case %+v is not tagged", c.Copy)
		}
		copies[c.Copy] = append(copies[c.Copy], c)
	}
	byKind := map[string]int{}
	sentences := map[string]bool{}
	pairs := map[string][]plantedCase{}
	families := map[string]bool{}
	for _, p := range manifest {
		byKind[p.Shape+" "+p.Kind]++
		pairs[p.Pair] = append(pairs[p.Pair], p)
		families[p.Family] = true
		if sentences[p.Sentence] {
			t.Errorf("sentence %q is planted twice", p.Sentence)
		}
		sentences[p.Sentence] = true
		if p.Family == "issue_title" || p.Tag != plantedTag || p.Seed != 11 {
			t.Errorf("planted case %s: family %s, tag %q, seed %d", p.Copy, p.Family, p.Tag, p.Seed)
		}
		orig := original[writeKey{EventID: p.EventID, SourceID: p.SourceID}]
		cp := copies[p.Copy]
		if len(cp) != len(orig) {
			t.Fatalf("copy %s has %d fields, the write %d", p.Copy, len(cp), len(orig))
		}
		changed := 0
		for i := range cp {
			if cp[i].Text == orig[i].Text {
				continue
			}
			changed++
			if cp[i].Field != p.Field {
				t.Errorf("copy %s changed field %s, not %s", p.Copy, cp[i].Field, p.Field)
			}
			starts := paragraphStarts(orig[i].Text)
			if len(starts) != p.Boundaries {
				t.Fatalf("copy %s: %d paragraph starts, the manifest says %d", p.Copy, len(starts), p.Boundaries)
			}
			want, at := insertParagraph(orig[i].Text, starts[p.Boundary], p.Sentence)
			if at != p.Position {
				t.Errorf("copy %s: the sentence is at %d, the manifest says %d", p.Copy, at, p.Position)
			}
			if cp[i].Text != want || cp[i].Text[p.Position:p.Position+len(p.Sentence)] != p.Sentence {
				t.Errorf("copy %s is not the write with one paragraph planted at %d:\n%s", p.Copy, p.Position, cp[i].Text)
			}
			if cp[i].SHA256 != sha256Hex([]byte(cp[i].Text)) {
				t.Errorf("copy %s keeps the original's digest", p.Copy)
			}
			if n := count(prose.ProseSentences(cp[i].Field, cp[i].Text), p.SentenceProse); n != 1 {
				t.Errorf("copy %s reads the planted sentence %d times", p.Copy, n)
			}
			if !strings.Contains(cp[i].Text, "```sh\necho hi\n\necho bye\n```") && cp[i].Surface == surfaceDocument {
				t.Errorf("copy %s broke the code block", p.Copy)
			}
		}
		if changed != 1 {
			t.Errorf("copy %s changes %d fields, want 1", p.Copy, changed)
		}
	}
	for _, shape := range []string{shapeNames, shapeShows, shapePays} {
		if byKind[shape+" fault"] != 6 || byKind[shape+" control"] != 6 {
			t.Errorf("%s: %d faults, %d controls; want 6 and 6", shape, byKind[shape+" fault"], byKind[shape+" control"])
		}
	}
	if byKind["other fault"] != 8 || byKind["other control"] != 8 {
		t.Errorf("other: %d faults, %d controls; want 8 and 8", byKind["other fault"], byKind["other control"])
	}
	for id, pair := range pairs {
		if len(pair) != 2 || pair[0].Kind != kindFault || pair[1].Kind != kindControl ||
			pair[0].Verb != pair[1].Verb || pair[0].EventID != pair[1].EventID || pair[0].SourceID != pair[1].SourceID ||
			pair[0].Position != pair[1].Position {
			t.Errorf("pair %s is not a fault and its matched control in one write at one place: %+v", id, pair)
		}
	}
	if len(families) != 4 {
		t.Errorf("planted in families %v, want the 4 with a body", families)
	}
}

func TestRelayedStartsMarksStartsNextToRelayedOrQuotedWords(t *testing.T) {
	text := "Intro para.\n\nKim wrote:\n\nWe should ship it.\n\nSecond relayed para.\n\n## Next\n\nPlain one.\n\n" +
		"> quoted\n\nAfter quote.\n\nHe said \"go\".\n\nLast."
	starts := paragraphStarts(text)
	got := relayedStarts(text, starts)
	want := []bool{false, false, true, true, true, false, true, true, true, true, true}
	if !slices.Equal(got, want) {
		var opens []string
		for _, s := range starts {
			opens = append(opens, strings.SplitN(text[s:], "\n", 2)[0])
		}
		t.Errorf("relayed starts = %v, want %v (starts open %q)", got, want, opens)
	}
}

func TestPlaceFallsBackToTheFirstStartWhenEveryStartIsRelayed(t *testing.T) {
	cases := []Case{{Field: "body", Surface: surfaceComment, Text: "\"Ship it,\" the reviewer wrote.\n\nThen \"more\" came."}}
	ref := &writeRef{Key: writeKey{EventID: 1, SourceID: "c"}, Family: "comment"}
	rng := rand.New(rand.NewPCG(1, 2))
	copies, rows, ok := place(cases, ref, []string{"The new section names the risk."}, rng)
	if !ok || rows[0].Boundary != 0 || rows[0].Position != 0 || !rows[0].NextToRelayed {
		t.Fatalf("place = ok %v, rows %+v", ok, rows)
	}
	if !strings.HasPrefix(copies[0][0].Text, "The new section names the risk.\n\n\"Ship it,\"") {
		t.Errorf("copy = %q", copies[0][0].Text)
	}
	// Only the start before "Kim wrote:" is clear of the relayed words.
	cases[0].Text = "Kim wrote:\n\nWe should ship it.\n\nThe build passed on Monday."
	for seed := range uint64(20) {
		_, rows, ok := place(cases, ref, []string{"The new section names the risk."}, rand.New(rand.NewPCG(seed, 1)))
		if !ok || rows[0].NextToRelayed || rows[0].Boundary != 0 {
			t.Errorf("seed %d: planted at start %d (relayed %v), want start 0", seed, rows[0].Boundary, rows[0].NextToRelayed)
		}
	}
}
