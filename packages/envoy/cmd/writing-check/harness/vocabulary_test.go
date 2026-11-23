package harness

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

// ownerText is one line of an owner-texts.jsonl.gz, as `writing-check dataset owner-texts`
// writes it: a text the owner wrote himself, with its time.
func ownerText(at, text string) CaseText {
	return CaseText{Key: "comment:o", Kind: "comment", ActorKind: "user", Actor: fixtureOwner, Time: at, Text: text}
}

// writeOwnerTexts writes texts as an owner-texts.jsonl.gz and answers its path.
func writeOwnerTexts(t *testing.T, texts ...CaseText) string {
	t.Helper()
	lines := make([]any, len(texts))
	for i, text := range texts {
		lines[i] = text
	}
	path := filepath.Join(t.TempDir(), "owner-texts.jsonl.gz")
	writeGzipLines(t, path, lines...)
	return path
}

func moment(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// The vocabulary's time cut and its word matching: a word counts only where the owner wrote it
// in a text dated before the case, never at its time or after it; a word matches whatever its
// case, its Markdown marks and the punctuation or the "'s" at its end, and a longer name that
// holds it is another word.
func TestTheVocabularyKnowsOnlyTheWordsWrittenBeforeTheCase(t *testing.T) {
	const first, question = "2026-11-01T10:00:00Z", "2026-11-03T10:00:00.5Z"
	// The file's order is not the texts' order in time: the first time a word was written decides.
	v, err := ReadVocabulary(writeOwnerTexts(t,
		ownerText(question, "What is the flim gate?"),
		ownerText(first, "The Zork train is late. Run it again, 40 times."),
	))
	if err != nil {
		t.Fatal(err)
	}
	text := "The **Zork** train's flim gate is shut; ZORK-7 waits 40 s for `run.sh`. The zork is slow."
	for _, tc := range []struct {
		name string
		at   string
		want []string
	}{
		{"before his first text", "2026-11-01T09:59:59Z", nil},
		{"at the time of his first text", first, nil},
		{"after his first text, before his question", "2026-11-02T00:00:00Z", []string{"the", "zork", "train", "is"}},
		{"at the time of his question", question, []string{"the", "zork", "train", "is"}},
		{"after his question", "2026-11-03T10:00:00.6Z", []string{"the", "zork", "train", "flim", "gate", "is"}},
	} {
		if got := v.Known(text, moment(t, tc.at)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: known = %q, want %q", tc.name, got, tc.want)
		}
	}
	// A longer name that holds a word he wrote is not that word, and neither is a number.
	if got := v.Known("ZORK-7 runs run.sh 40 times", moment(t, "2026-11-02T00:00:00Z")); !slices.Equal(got, []string{"times"}) {
		t.Errorf("known = %q, want only \"times\"", got)
	}
	// A text whose time cannot be read is refused: its words have no place in time.
	if _, err := ReadVocabulary(writeOwnerTexts(t, ownerText("yesterday", "The zork."))); err == nil || !strings.Contains(err.Error(), "the time of a text") {
		t.Errorf("a text with no time: %v", err)
	}
}

// With --vocabulary the undefined judgement reads, for each case, the words of the case's text
// that the owner wrote before the case, and its prompt says such a word is not undefined for
// him. Without the flag no call holds them.
func TestTheUndefinedJudgementReadsTheOwnersOwnWords(t *testing.T) {
	build, sessions := writeFixture(t)
	var cases []any
	for _, c := range fixtureCases() {
		c.WriteTime = "2026-11-02T00:00:00Z"
		cases = append(cases, c)
	}
	writeGzipLines(t, filepath.Join(build, casesFile), cases...)
	owner := writeOwnerTexts(t,
		ownerText("2026-11-01T10:00:00Z", "The Zork train is late."),
		ownerText("2026-11-02T00:00:00Z", "What does \"merges\" mean?"),
		ownerText("2026-11-03T10:00:00Z", "Is it ready at noon?"),
	)
	score := func(extra ...string) (*modeltest.Model, string) {
		stand := modeltest.New(t, graderRules(t)...)
		out := t.TempDir()
		args := append([]string{"score", "--cases", build, "--sessions", sessions, "--out", out, "--judgements", "undefined",
			"--gateway", stand.URL, "--gateway-key-file", keyFile(t), "--draws", "50", "--exposure-log", filepath.Join(t.TempDir(), "exposure.log")}, extra...)
		if code, _, stderr := runHarness(args...); code != 0 {
			t.Fatalf("score %v = %d: %s", extra, code, stderr)
		}
		return stand, out
	}
	// request is the undefined call of the question case, whose text is "The zork train is ready.
	// It merges at noon."
	request := func(stand *modeltest.Model) string {
		t.Helper()
		for _, text := range requestsOf(stand, markUndefined) {
			if strings.Contains(text, "The zork train is ready.") {
				return text
			}
		}
		t.Fatal("no undefined call of the question case")
		return ""
	}

	stand, out := score("--vocabulary", owner)
	with := request(stand)
	// "merges" he wrote at the case's own time, and "ready", "at" and "noon" after it.
	for _, want := range []string{
		"<reader_own_words>\n- the\n- zork\n- train\n- is\n</reader_own_words>",
		`- "reader_own_words": the words of the text that the reader wrote himself`,
		`- It is in "reader_own_words": the reader wrote it himself before, so it is not undefined for him.`,
		`a person wrote it in the record, it is in "reader_own_words", or the record uses it`,
	} {
		if !strings.Contains(with, want) {
			t.Errorf("with --vocabulary, the undefined call does not hold %q:\n%s", want, with)
		}
	}
	data, err := os.ReadFile(filepath.Join(out, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	if sum, err := fileSHA256(owner); err != nil || !strings.Contains(string(data), `"vocabulary_sha256": "`+sum+`"`) {
		t.Errorf("run.json does not record the vocabulary %s (%v): %s", sum, err, data)
	}

	stand, out = score()
	without := request(stand)
	if strings.Contains(without, "reader_own_words") {
		t.Errorf("without --vocabulary, the undefined call holds the reader's own words:\n%s", without)
	}
	if !strings.Contains(without, "a person wrote it in the record, or the record uses it itself as a known term of the work, else false.") {
		t.Errorf("without --vocabulary, the undefined prompt is not the one it was:\n%s", without)
	}
	if data, _ := os.ReadFile(filepath.Join(out, "run.json")); strings.Contains(string(data), "vocabulary_sha256") {
		t.Errorf("run.json of a run with no vocabulary: %s", data)
	}
	if code, _, stderr := runHarness("score", "--cases", build, "--sessions", sessions, "--out", t.TempDir(), "--vocabulary", filepath.Join(t.TempDir(), "none.jsonl.gz"),
		"--gateway", stand.URL, "--gateway-key-file", keyFile(t)); code != 2 {
		t.Errorf("a vocabulary that does not exist = %d: %s", code, stderr)
	}
}
