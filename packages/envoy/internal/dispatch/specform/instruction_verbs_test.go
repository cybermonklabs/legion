package specform

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// The program that writes the list of the verbs of an instruction (instruction-verbs.txt) and its
// evidence (testdata/instruction-verbs.json), and the test that holds both to it. Run with
// -write-verbs, it writes the list and its evidence from the two source files the test reads: the
// sentences of the repository's skills (skills/*/SKILL.md) kept in
// testdata/instruction-verb-sources.json, and the live procedures of The Spec kept in
// testdata/live-procedures.txt, so it writes them again in any clone, also one of depth 1, and
// writes the list that the test accepts. With -verbs-commit it first reads the skills again from
// git at that commit and keeps their sentences in the sources file; with -verbs-live (a `dispatch
// doc-read` of the live Verification document of The Spec's root) it first keeps that document's
// procedures. From the two files it writes each word that opens an instruction sentence
// (openerVerb). Run with no flag, the test builds the evidence and the list again from the same
// two files, in any clone, and fails when either file differs, so an edit of either by hand fails;
// it fails by name when a source file is missing or empty. The commit is the sources' provenance,
// and the command is the list's header. The program writes every Dispatch key of a sentence as
// `<KEY>` (dispatchKeyForm), the skills' own word for a key, before it keeps the sentence, so
// neither file names an issue; the test fails when one does.
var (
	writeVerbs  = flag.Bool("write-verbs", false, "write instruction-verbs.txt and its evidence from the kept sources, after -verbs-commit and -verbs-live")
	verbsCommit = flag.String("verbs-commit", "", "read the skills again from git at this commit and keep their sentences as the sources; the kept sources when empty")
	verbsLive   = flag.String("verbs-live", "", "a `dispatch doc-read` of the Verification document whose procedures the list reads; the kept procedures when empty")
)

const (
	verbsListFile     = "instruction-verbs.txt"
	verbsEvidenceFile = "testdata/instruction-verbs.json"
	verbsSourcesFile  = "testdata/instruction-verb-sources.json"
	verbsLiveFile     = "testdata/live-procedures.txt"
	verbsRepository   = "../../../../.."
)

// verbEvidence is the list's evidence: the source commit and the command, each source with its
// digest and the sentences of it that open an instruction, and one sentence for each listed verb
// that a source also uses as a noun (after a determiner).
type verbEvidence struct {
	Commit  string       `json:"commit"`
	Command string       `json:"command"`
	Sources []verbSource `json:"sources"`
	Verbs   []string     `json:"verbs"`
	Nouns   []verbNoun   `json:"also_nouns"`
}

type verbSource struct {
	Path      string        `json:"path"`
	SHA256    string        `json:"sha256"`
	Sentences int           `json:"sentences"`
	Openers   []verbOpening `json:"openers"`
}

// verbOpening is a sentence of a source that opens an instruction, and its verb.
type verbOpening struct {
	Word     string `json:"word"`
	Line     int    `json:"line"`
	Sentence string `json:"sentence"`
}

// verbNoun is a listed verb that a source also uses as a noun, and the sentence that does, with
// the path of its source and its line each in a field of its own, so no check reads a place out
// of the sentence's words.
type verbNoun struct {
	Word     string `json:"word"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Sentence string `json:"sentence"`
}

// sourceSentence is one sentence of a source as the program reads it: the line it starts on and
// its words.
type sourceSentence struct {
	Line     int    `json:"line"`
	Sentence string `json:"sentence"`
}

// skillText is one skill at the source commit as testdata/instruction-verb-sources.json keeps it:
// its path, the digest of the file, and each of its sentences.
type skillText struct {
	Path      string           `json:"path"`
	SHA256    string           `json:"sha256"`
	Sentences []sourceSentence `json:"sentences"`
}

// notVerbs are words that can open a sentence and stand before an object start and are no verb:
// the closed classes of English (articles, quantities, pronouns, prepositions, conjunctions,
// auxiliaries, numbers, adverbs and question words). The program reads a verb only where a sentence
// opens with a word outside them; the rule reads only the words the program lists.
var notVerbs = wordSet(
	"the", "a", "an", "each", "every", "all", "any", "both", "no", "some", "most", "many", "few", "more", "less", "other", "another",
	"its", "their", "his", "her", "our", "your", "my", "this", "these", "those", "that", "which", "who", "whom", "whose", "what",
	"it", "they", "them", "we", "us", "you", "he", "him", "she", "i", "me", "there", "here",
	"nobody", "anybody", "somebody", "everybody", "nothing", "anything", "something", "everything",
	"nowhere", "anywhere", "somewhere", "everywhere", "either", "neither", "otherwise",
	"of", "in", "on", "at", "to", "from", "for", "with", "by", "as", "into", "onto", "over", "under", "about", "against",
	"between", "through", "without", "within", "beside", "after", "before", "during", "since", "per", "via", "than", "past",
	"inside", "outside", "around", "across", "along", "among", "behind", "below", "above", "beyond", "near", "off", "out", "up",
	"down", "toward", "towards", "upon", "throughout", "except", "like", "unlike", "despite", "beneath",
	"and", "or", "but", "nor", "so", "if", "when", "where", "while", "once", "unless", "until", "because", "then",
	"whether", "how", "why", "whatever", "whichever", "wherever", "whenever",
	"is", "are", "was", "were", "be", "been", "am", "do", "does", "did", "has", "have", "had", "can", "could", "may", "might",
	"must", "shall", "should", "will", "would", "not", "also", "only", "too", "very", "just", "still", "even", "first",
	"never", "always", "often", "sometimes", "usually", "again", "now", "later", "soon", "rather", "instead", "else", "already",
	"one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve")

// determiners stand before a noun, so a listed verb after one is a noun there.
var determiners = wordSet("the", "a", "an", "each", "every", "its", "their", "this", "these", "those", "any", "no", "your", "our")

// openerVerb is the verb that opens the sentence of raw (each word as the line writes it) as an
// instruction, when it does: its first word starts with a capital letter, is a word of letters
// alone with no mark after it, is in no closed class (notVerbs) and in no form of another person
// or time (-s, -ed, -ing), and stands before an object start that opens no clause of its own
// (contactClause), not inside brackets.
func openerVerb(raw []string) (string, bool) {
	if len(raw) < 2 {
		return "", false
	}
	first := strings.Trim(raw[0], "*_")
	if first == "" || !unicode.IsUpper([]rune(first)[0]) || strings.ContainsFunc(first, func(r rune) bool { return !unicode.IsLetter(r) }) ||
		strings.HasPrefix(strings.TrimLeft(raw[1], "*_"), "(") {
		return "", false
	}
	trimmed := make([]string, len(raw))
	for index, word := range raw {
		trimmed[index] = strings.Trim(word, "*_")
	}
	words, commas := sentenceWords(trimmed)
	verb := words[0]
	if notVerbs[verb] || commas[0] || !objectStarts[words[1]] || contactClause(words, commas, 0) ||
		strings.HasSuffix(verb, "ing") || strings.HasSuffix(verb, "ed") ||
		(strings.HasSuffix(verb, "s") && !strings.HasSuffix(verb, "ss") && !strings.HasSuffix(verb, "us") && !strings.HasSuffix(verb, "is")) {
		return "", false
	}
	return verb, true
}

var (
	listMarker = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)]|#+|>)\s+`)
	fence      = regexp.MustCompile("^\\s*(```|~~~)")
	// dispatchKeyForm is a Dispatch issue key: its project's capitals, a hyphen, then its number.
	dispatchKeyForm = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*\b`)
)

// markdownSentences is each sentence of a Markdown file, with the line it starts on: the lines of
// a paragraph or of a list item are read as one, a heading, a list item, a quote and a table row
// each start one of their own, and a fenced block is read as no prose.
func markdownSentences(markdown string) []sourceSentence {
	var found []sourceSentence
	var unit strings.Builder
	start := 0
	flush := func() {
		coded, spans := codeAsWords(strings.TrimSpace(unit.String()))
		for _, sentence := range sentencesOf(coded) {
			found = append(found, sourceSentence{Line: start, Sentence: spans.Replace(strings.Join(sentence.words, " "))})
		}
		unit.Reset()
	}
	fenced := false
	for number, line := range strings.Split(markdown, "\n") {
		if fence.MatchString(line) {
			flush()
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		blank := strings.TrimSpace(line) == ""
		marker := listMarker.MatchString(line) || strings.HasPrefix(strings.TrimSpace(line), "|")
		if blank || marker {
			flush()
		}
		if blank {
			continue
		}
		if unit.Len() == 0 {
			start = number + 1
		}
		for listMarker.MatchString(line) {
			line = listMarker.ReplaceAllString(line, "")
		}
		unit.WriteString(" " + strings.TrimSpace(line))
	}
	flush()
	return found
}

// sentenceVerb is the opener of one sentence as the evidence keeps it, its spans of code font
// written as one word again, as the program read it.
func sentenceVerb(sentence string) (string, bool) {
	coded, _ := codeAsWords(sentence)
	return openerVerb(strings.Fields(coded))
}

// readSkills reads each skills/*/SKILL.md at commit: its digest, and each of its sentences, with
// every Dispatch key written as `<KEY>`. Only -verbs-commit reads git; -write-verbs with no commit
// and the test read what this kept (verbsSourcesFile).
func readSkills(t *testing.T, commit string) []skillText {
	t.Helper()
	git := func(args ...string) []byte {
		out, err := exec.Command("git", append([]string{"-C", verbsRepository}, args...)...).Output()
		if err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
		return out
	}
	var skills []skillText
	for _, path := range strings.Fields(string(git("ls-tree", "-r", "--name-only", commit, "--", "skills"))) {
		if matched, _ := filepath.Match("skills/*/SKILL.md", path); !matched {
			continue
		}
		text := git("show", commit+":"+path)
		digest := sha256.Sum256(text)
		skill := skillText{Path: path, SHA256: hex.EncodeToString(digest[:]), Sentences: []sourceSentence{}}
		for _, sentence := range markdownSentences(string(text)) {
			sentence.Sentence = dispatchKeyForm.ReplaceAllString(sentence.Sentence, "<KEY>")
			skill.Sentences = append(skill.Sentences, sentence)
		}
		skills = append(skills, skill)
	}
	if len(skills) == 0 {
		t.Fatalf("UNKNOWN: commit %s holds no skills/*/SKILL.md", commit)
	}
	return skills
}

// buildEvidence is the evidence of the list from the skills as kept and the kept live procedures
// (live), for commit: each source's sentences that open an instruction, the verbs they open
// with, and the first sentence of any source that uses each verb as a noun.
func buildEvidence(commit string, skills []skillText, live []byte) verbEvidence {
	var procedures []sourceSentence
	for number, procedure := range strings.Split(strings.TrimSpace(string(live)), "\n") {
		for _, sentence := range markdownSentences(procedure) {
			procedures = append(procedures, sourceSentence{Line: number + 1, Sentence: sentence.Sentence})
		}
	}
	digest := sha256.Sum256(live)
	sources := append(slices.Clone(skills), skillText{Path: verbsLiveFile, SHA256: hex.EncodeToString(digest[:]), Sentences: procedures})
	evidence := verbEvidence{Commit: commit, Command: verbsCommand}
	verbs := map[string]bool{}
	for _, source := range sources {
		read := verbSource{Path: source.Path, SHA256: source.SHA256, Sentences: len(source.Sentences), Openers: []verbOpening{}}
		for _, sentence := range source.Sentences {
			text := dispatchKeyForm.ReplaceAllString(sentence.Sentence, "<KEY>")
			if verb, ok := sentenceVerb(text); ok {
				read.Openers = append(read.Openers, verbOpening{Word: verb, Line: sentence.Line, Sentence: text})
				verbs[verb] = true
			}
		}
		evidence.Sources = append(evidence.Sources, read)
	}
	for verb := range verbs {
		evidence.Verbs = append(evidence.Verbs, verb)
	}
	slices.Sort(evidence.Verbs)
	nouns := map[string]bool{}
	for _, source := range sources {
		for _, sentence := range source.Sentences {
			text := dispatchKeyForm.ReplaceAllString(sentence.Sentence, "<KEY>")
			if word, ok := nounUse(text, verbs); ok && !nouns[word] {
				nouns[word] = true
				evidence.Nouns = append(evidence.Nouns, verbNoun{Word: word, Path: source.Path, Line: sentence.Line, Sentence: text})
			}
		}
	}
	slices.SortFunc(evidence.Nouns, func(a, b verbNoun) int { return strings.Compare(a.Word, b.Word) })
	return evidence
}

// nounUse is the first listed verb of sentence that stands right after a determiner, where it is a
// noun: "the record", "its name".
func nounUse(sentence string, verbs map[string]bool) (string, bool) {
	coded, _ := codeAsWords(sentence)
	words, _ := sentenceWords(strings.Fields(coded))
	for index := 1; index < len(words); index++ {
		if determiners[words[index-1]] && verbs[words[index]] {
			return words[index], true
		}
	}
	return "", false
}

// verbsCommand is the command that writes the list and its evidence again from the kept sources,
// which the list's header and each failure of the test print.
const verbsCommand = "go test ./internal/dispatch/specform/ -run '^TestTheVerbListIsItsProgramsOutput$' -args -write-verbs"

// renderVerbs is the list file of evidence.
func renderVerbs(evidence verbEvidence) []byte {
	var out bytes.Buffer
	fmt.Fprintf(&out, "# The verbs of an instruction (R49.2): each word that opens an instruction sentence of the sources, one a line.\n")
	fmt.Fprintf(&out, "# Written by its program (instruction_verbs_test.go); do not edit. From packages/envoy:\n#   %s\n", evidence.Command)
	fmt.Fprintf(&out, "# Sources: skills/*/SKILL.md at %s, kept in %s, and the procedures of %s. Evidence: %s.\n",
		evidence.Commit, verbsSourcesFile, verbsLiveFile, verbsEvidenceFile)
	for _, verb := range evidence.Verbs {
		out.WriteString(verb + "\n")
	}
	return out.Bytes()
}

func renderJSON(t *testing.T, value any) []byte {
	t.Helper()
	out, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

// evidenceProblems is each opener of evidence whose sentence does not open with its verb, and each
// noun whose sentence does not hold it after a determiner, read from the sentence's own field.
func evidenceProblems(evidence verbEvidence) []string {
	var problems []string
	for _, source := range evidence.Sources {
		for _, opener := range source.Openers {
			if verb, ok := sentenceVerb(opener.Sentence); !ok || verb != opener.Word {
				problems = append(problems, fmt.Sprintf("%s:%d: %q opens with %q, %v, and the evidence says %q",
					source.Path, opener.Line, opener.Sentence, verb, ok, opener.Word))
			}
		}
	}
	for _, noun := range evidence.Nouns {
		if word, ok := nounUse(noun.Sentence, map[string]bool{noun.Word: true}); !ok || word != noun.Word || !instructionVerbs[noun.Word] {
			problems = append(problems, fmt.Sprintf("%s:%d: %q is no listed verb after a determiner in %q", noun.Path, noun.Line, noun.Word, noun.Sentence))
		}
	}
	return problems
}

// writeLiveProcedures keeps the procedures of the live Verification document, one a line.
func writeLiveProcedures(t *testing.T, path string) {
	t.Helper()
	markdown, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	read, err := ExtractDocument("verification.md", 1, string(markdown))
	if err != nil {
		t.Fatal(err)
	}
	var procedures []string
	for _, text := range read.Texts {
		if text.Kind == KindProcedure {
			procedures = append(procedures, strings.Join(strings.Fields(text.Body), " "))
		}
	}
	if len(procedures) == 0 {
		t.Fatalf("UNKNOWN: %s holds no procedure", path)
	}
	if err := os.WriteFile(verbsLiveFile, []byte(strings.Join(procedures, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTheVerbListIsItsProgramsOutput(t *testing.T) {
	if *writeVerbs {
		if *verbsLive != "" {
			writeLiveProcedures(t, *verbsLive)
		}
		commit := *verbsCommit
		if commit != "" {
			if err := os.WriteFile(verbsSourcesFile, renderJSON(t, readSkills(t, commit)), 0o644); err != nil {
				t.Fatal(err)
			}
		} else {
			commit = keptCommit(t)
		}
		skills, _, live := keptSources(t)
		evidence := buildEvidence(commit, skills, live)
		for path, content := range map[string][]byte{
			verbsEvidenceFile: renderJSON(t, evidence),
			verbsListFile:     renderVerbs(evidence),
		} {
			if err := os.WriteFile(path, content, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	raw, evidence := keptEvidence(t)
	skills, kept, live := keptSources(t)
	// Neither file names a Dispatch issue: the program writes each key as `<KEY>`.
	for path, content := range map[string][]byte{verbsEvidenceFile: raw, verbsSourcesFile: kept} {
		if keys := dispatchKeyForm.FindAll(content, -1); len(keys) > 0 {
			t.Errorf("%s holds %d Dispatch keys (%s): regenerate it with %s -verbs-commit <commit>", path, len(keys), bytes.Join(keys, []byte(", ")), verbsCommand)
		}
	}
	built := buildEvidence(evidence.Commit, skills, live)
	if got := renderJSON(t, built); !bytes.Equal(raw, got) {
		t.Errorf("%s is not its program's output from %s and %s: regenerate it with %s", verbsEvidenceFile, verbsSourcesFile, verbsLiveFile, verbsCommand)
	}
	if got := renderVerbs(built); !bytes.Equal([]byte(instructionVerbsFile), got) {
		t.Errorf("%s is not its program's output from %s and %s: regenerate it with %s", verbsListFile, verbsSourcesFile, verbsLiveFile, verbsCommand)
	}
	if len(instructionVerbs) != len(built.Verbs) {
		t.Errorf("the sources give %d verbs, and the rule reads %d", len(built.Verbs), len(instructionVerbs))
	}
	for _, problem := range evidenceProblems(evidence) {
		t.Error(problem)
	}
}

// keptEvidence is the list's evidence as the tree keeps it, and its bytes.
func keptEvidence(t *testing.T) ([]byte, verbEvidence) {
	t.Helper()
	raw, err := os.ReadFile(verbsEvidenceFile)
	if err != nil {
		t.Fatal(err)
	}
	var evidence verbEvidence
	if err := json.Unmarshal(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	return raw, evidence
}

// keptCommit is the commit that the kept sources were read at, as the evidence names it: their
// provenance, which -write-verbs keeps when it reads no skill again.
func keptCommit(t *testing.T) string {
	t.Helper()
	if _, evidence := keptEvidence(t); evidence.Commit != "" {
		return evidence.Commit
	}
	t.Fatalf("UNKNOWN: %s names no commit of its sources: read the skills again with %s -verbs-commit <commit>", verbsEvidenceFile, verbsCommand)
	return ""
}

// keptSources is the sources the list is built from, which the tree holds, so the list is built
// again in any clone, also one that does not hold the commit the skills were read at: the skills'
// sentences (and the bytes of their file) and the live procedures. A missing or empty source is a
// failure by name, never a pass of a test that saw nothing.
func keptSources(t *testing.T) ([]skillText, []byte, []byte) {
	t.Helper()
	kept, err := os.ReadFile(verbsSourcesFile)
	if err != nil {
		t.Fatalf("UNKNOWN: the list's sources %s cannot be read (%v): read the skills again with %s -verbs-commit <commit>", verbsSourcesFile, err, verbsCommand)
	}
	var skills []skillText
	if err := json.Unmarshal(kept, &skills); err != nil || len(skills) == 0 {
		t.Fatalf("UNKNOWN: %s holds no skill (%v): read the skills again with %s -verbs-commit <commit>", verbsSourcesFile, err, verbsCommand)
	}
	live, err := os.ReadFile(verbsLiveFile)
	if err != nil || len(strings.TrimSpace(string(live))) == 0 {
		t.Fatalf("UNKNOWN: the live procedures %s cannot be read or are empty (%v)", verbsLiveFile, err)
	}
	return skills, kept, live
}

// The checks of the evidence read each sentence from its own field, never a place written into
// its words: a source's path written another way leaves every check as it was, and a sentence
// changed in its words fails its check.
func TestTheVerbEvidenceChecksReadTheSentenceAlone(t *testing.T) {
	raw, err := os.ReadFile(verbsEvidenceFile)
	if err != nil {
		t.Fatal(err)
	}
	var evidence verbEvidence
	if err := json.Unmarshal(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	if len(evidence.Nouns) == 0 || len(evidence.Sources) == 0 {
		t.Fatalf("UNKNOWN: %s holds no noun or no source", verbsEvidenceFile)
	}
	if problems := evidenceProblems(evidence); len(problems) != 0 {
		t.Fatalf("the evidence as kept: %q", problems)
	}
	moved := evidence
	moved.Sources = slices.Clone(evidence.Sources)
	moved.Nouns = slices.Clone(evidence.Nouns)
	for index := range moved.Sources {
		moved.Sources[index].Path = "./" + moved.Sources[index].Path + ": "
	}
	for index := range moved.Nouns {
		moved.Nouns[index].Path = "./" + moved.Nouns[index].Path + ": "
	}
	if problems := evidenceProblems(moved); len(problems) != 0 {
		t.Errorf("the paths written another way change the checks: %q", problems)
	}
	changed := evidence
	changed.Nouns = slices.Clone(evidence.Nouns)
	changed.Nouns[0].Sentence = "A sentence without that word."
	if problems := evidenceProblems(changed); len(problems) != 1 {
		t.Errorf("a noun's sentence changed in its words gives %d problems, want 1: %q", len(problems), problems)
	}
}
