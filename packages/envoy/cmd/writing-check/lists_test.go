package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `dataset lists` writes the three case lists of the development part, each in id order with its
// SHA-256: the question cases whose verdict says about_words true, every answer case, and the plain
// cases whose final label is understood. A held-out case, a phrase case, a question case about
// other things or unlabelled, and a plain case the owner did not understand are in no list. Every
// id and label below is written for this test.
func TestDatasetListsWritesTheThreeCaseLists(t *testing.T) {
	dir := t.TempDir()
	build := filepath.Join(dir, "build")
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatal(err)
	}
	c := func(id, group, part string) dsListCase { return dsListCase{ID: id, Group: group, Part: part} }
	cases := []dsListCase{
		c("question/q3", dsGroupQuestion, dsDevelopment), c("question/q1", dsGroupQuestion, dsDevelopment),
		c("question/q2", dsGroupQuestion, dsDevelopment), c("question/q4", dsGroupQuestion, dsDevelopment),
		c("question/qh", dsGroupQuestion, dsHeldOut),
		c("answer/a2", dsGroupAnswer, dsDevelopment), c("answer/a1", dsGroupAnswer, dsDevelopment), c("answer/ah", dsGroupAnswer, dsHeldOut),
		c("plain/p1", dsGroupPlain, dsDevelopment), c("plain/p2", dsGroupPlain, dsDevelopment), c("plain/p3", dsGroupPlain, dsDevelopment),
		c("plain/p4", dsGroupPlain, dsDevelopment), c("plain/ph", dsGroupPlain, dsHeldOut),
		c("phrase/f1", dsGroupPhrase, dsDevelopment),
	}
	if _, err := dsWriteGzipJSONL(filepath.Join(build, "cases.jsonl.gz"), cases); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	verdicts := filepath.Join(dir, "verdicts")
	writeJSONLines(t, verdicts,
		map[string]any{"id": "question/q1", "about_words": yes, "status": "agreed", "part": dsDevelopment},
		map[string]any{"id": "question/q2", "about_words": no, "status": "agreed", "part": dsDevelopment},
		map[string]any{"id": "question/q3", "status": "unlabelled"},
		map[string]any{"id": "question/qh", "about_words": yes, "status": "settled", "part": dsHeldOut},
	)
	labels := filepath.Join(dir, "labels-final.jsonl")
	writeJSONLines(t, labels,
		map[string]any{"case_id": "plain/p1", "label": "acted", "final": "understood", "part": dsDevelopment},
		map[string]any{"case_id": "plain/p2", "label": "question_or_complaint", "final": "not_plain", "part": dsDevelopment},
		map[string]any{"case_id": "plain/p3", "label": "silent", "final": "silent", "part": dsDevelopment},
		map[string]any{"id": "plain/p4", "label": "question_or_complaint", "final": "understood", "part": dsDevelopment},
		map[string]any{"case_id": "plain/ph", "label": "acted", "final": "understood", "part": dsHeldOut},
	)
	out := filepath.Join(dir, "lists")
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"dataset", "lists", "--cases", build, "--verdicts", verdicts, "--plain-labels", labels, "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("dataset lists = %d:\n%s", code, stderr.String())
	}
	for _, want := range []struct{ name, ids, says string }{
		{dsListQuestion, "question/q1\n", "the development question cases whose question is about the agent's words: 1 of 4 (about_words: false 1, no verdict 1, true 1, unlabelled 1)"},
		{dsListAnswer, "answer/a1\nanswer/a2\n", "the development answer cases: 2"},
		{dsListPlain, "plain/p1\nplain/p4\n", "the development plain cases the owner understood (final label understood): 2 of 4 (final: not_plain 1, silent 1, understood 2; label: acted 1, question_or_complaint 2, silent 1)"},
	} {
		data, err := os.ReadFile(filepath.Join(out, want.name))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want.ids {
			t.Errorf("%s holds %q, want %q", want.name, data, want.ids)
		}
		if printed := want.name + ": " + want.says + "\n" + want.name + " sha256: " + sha256Hex(data) + "\n"; !strings.Contains(stdout.String(), printed) {
			t.Errorf("dataset lists printed\n%s\nwant %q", stdout.String(), printed)
		}
	}

	// Labels of another build, of another part than the build's, or with no final label are
	// refused, and so is a call that names no input.
	for _, bad := range []struct {
		name, file string
		line       map[string]any
		says       string
	}{
		{"a verdict of a case the build does not hold", verdicts, map[string]any{"id": "question/q9", "about_words": yes, "part": dsDevelopment}, `case "question/q9" is not in`},
		{"a verdict of another part", verdicts, map[string]any{"id": "question/q1", "about_words": yes, "part": dsHeldOut}, "puts case question/q1 in the held_out part"},
		{"a verdict of an answer case", verdicts, map[string]any{"id": "answer/a1", "about_words": yes, "part": dsDevelopment}, "case answer/a1 is of group answer, not question"},
		{"a plain label with no final label", labels, map[string]any{"case_id": "plain/p1", "label": "acted", "part": dsDevelopment}, "holds no final label"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			in := map[string]string{verdicts: verdicts, labels: labels}
			in[bad.file] = filepath.Join(t.TempDir(), "bad.jsonl")
			writeJSONLines(t, in[bad.file], bad.line)
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), []string{"dataset", "lists", "--cases", build, "--verdicts", in[verdicts], "--plain-labels", in[labels], "--out", t.TempDir()}, &stdout, &stderr)
			if code != 1 || !strings.Contains(stderr.String(), bad.says) {
				t.Errorf("dataset lists = %d, want 1 saying %q:\n%s", code, bad.says, stderr.String())
			}
		})
	}
	var discard bytes.Buffer
	if code := run(t.Context(), []string{"dataset", "lists", "--cases", build, "--out", out}, &discard, &discard); code != 2 {
		t.Errorf("dataset lists with no labels = %d, want 2", code)
	}
}
