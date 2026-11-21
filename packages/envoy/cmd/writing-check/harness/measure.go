package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// The two count judgements are measured against references other than the grader's own word:
// phrases against the sentence labels of writing-check's labeller rounds (the measured definition
// of a vacuous verb phrase, cmd/writing-check README "label"), and undefined against the words
// the owner himself asked about on the question cases (owner_reply.verdict.words).

// tokens is text as lowercase words: runs of letters, digits, '-', '_' and '.' inside a word.
func tokens(text string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' && r != '.'
	}) {
		if f = strings.Trim(f, "-_."); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// within is whether the words of part run, in order and next to each other, inside whole.
func within(part, whole []string) bool {
	if len(part) == 0 || len(part) > len(whole) {
		return false
	}
	for i := 0; i+len(part) <= len(whole); i++ {
		match := true
		for j, w := range part {
			if whole[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// LabelledSentence is one sentence of a labelled write and its label.
type LabelledSentence struct {
	ID      string `json:"id"`
	Field   string `json:"field"`
	Text    string `json:"text"`
	Verdict string `json:"verdict"`
}

// labelledWrite is one write of a labeller round: its text, field by field, and its sentences.
type labelledWrite struct {
	EventID   int64              `json:"event_id"`
	SourceID  string             `json:"source_id"`
	Family    string             `json:"family"`
	Group     string             `json:"group"`
	Text      string             `json:"-"`
	Sentences []LabelledSentence `json:"-"`
}

func (w labelledWrite) key() string { return fmt.Sprintf("%d/%s", w.EventID, w.SourceID) }

// readLabelledWrites reads a labeller round's directory: cases.jsonl (the write texts, one line
// for each field) and labels.jsonl (each write's sentences and their verdicts).
func readLabelledWrites(dir string) ([]labelledWrite, error) {
	type caseLine struct {
		EventID  int64  `json:"event_id"`
		SourceID string `json:"source_id"`
		Text     string `json:"text"`
		Group    string `json:"group"`
	}
	texts := map[string][]string{}
	groups := map[string]string{}
	if err := eachJSONLine(filepath.Join(dir, "cases.jsonl"), func(line []byte) error {
		var c caseLine
		if err := json.Unmarshal(line, &c); err != nil {
			return err
		}
		k := fmt.Sprintf("%d/%s", c.EventID, c.SourceID)
		texts[k] = append(texts[k], c.Text)
		groups[k] = c.Group
		return nil
	}); err != nil {
		return nil, err
	}
	var out []labelledWrite
	err := eachJSONLine(filepath.Join(dir, "labels.jsonl"), func(line []byte) error {
		var l struct {
			labelledWrite
			Sentences []LabelledSentence `json:"sentences"`
		}
		if err := json.Unmarshal(line, &l); err != nil {
			return err
		}
		w := l.labelledWrite
		w.Text, w.Group, w.Sentences = strings.Join(texts[w.key()], "\n\n"), groups[w.key()], l.Sentences
		if w.Text == "" {
			return fmt.Errorf("labels.jsonl names write %s, which cases.jsonl does not hold", w.key())
		}
		out = append(out, w)
		return nil
	})
	return out, err
}

// eachJSONLine calls fn with every non-blank line of the file at path.
func eachJSONLine(path string, fn func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			if ferr := fn(line); ferr != nil {
				return fmt.Errorf("%s: %w", path, ferr)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// labelForm is a quote as the labels' sentence splitter writes text: each inline code span is
// CODE and each bare address URL.
var (
	inlineCode = regexp.MustCompile("`[^`]*`")
	bareURL    = regexp.MustCompile(`https?://\S+`)
)

func labelForm(quote string) []string {
	return tokens(bareURL.ReplaceAllString(inlineCode.ReplaceAllString(quote, " CODE "), " URL "))
}

// longestRun is the length of the longest run of part's words, in order and next to each other,
// that whole holds.
func longestRun(part, whole []string) int {
	best := 0
	for i := range part {
		for j := range whole {
			n := 0
			for i+n < len(part) && j+n < len(whole) && part[i+n] == whole[j+n] {
				n++
			}
			best = max(best, n)
		}
	}
	return best
}

// placeQuote is the id of the sentence of sentences that holds quote: the one whose words hold
// the quote's words in order, else the one that holds the longest run of them in order, when
// that run is at least three words and seven in ten of the quote's ("" when none does).
func placeQuote(quote string, sentences []LabelledSentence) string {
	q := labelForm(quote)
	if len(q) == 0 {
		return ""
	}
	best, bestRun := "", 0
	for _, s := range sentences {
		st := tokens(s.Text)
		if within(q, st) {
			return s.ID
		}
		if n := longestRun(q, st); n > bestRun {
			best, bestRun = s.ID, n
		}
	}
	if bestRun >= 3 && float64(bestRun) >= 0.7*float64(len(q)) {
		return best
	}
	return ""
}

// phrasesRun is the run.json phrases-labels writes in its --out: the grader, the prompts'
// digest (PromptsDigest, which holds candidates.yaml) and the labels directory its results were
// graded with, so a rerun reuses them only for the same three.
type phrasesRun struct {
	Grader        string `json:"grader"`
	PromptsSHA256 string `json:"prompts_sha256"`
	Labels        string `json:"labels"`
}

// LabelResult is one labelled write the phrases judgement graded: the grade, the sentences its
// findings fall in, and the findings no sentence holds.
type LabelResult struct {
	Key       string   `json:"key"`
	Family    string   `json:"family"`
	Group     string   `json:"group"`
	Grade     Grade    `json:"grade"`
	Predicted []string `json:"predicted"`
	Unplaced  []string `json:"unplaced,omitempty"`
	Time      string   `json:"time"`
}

// PhrasesAgreement is the phrases judgement against the labels, sentence by sentence: its
// precision over the sentences it found, its recall over the sentences labelled vacuous, and
// the precision that counts a finding no sentence holds as false.
type PhrasesAgreement struct {
	Labels            string  `json:"labels"`
	Writes            int     `json:"writes"`
	Sentences         int     `json:"sentences"`
	Vacuous           int     `json:"vacuous"`
	Precision         Share   `json:"precision"`
	PrecisionUnplaced Share   `json:"precision_counting_unplaced"`
	Recall            Share   `json:"recall"`
	Unplaced          int     `json:"unplaced"`
	CostUSD           float64 `json:"cost_usd"`
	InputTokens       int     `json:"input_tokens"`
	OutputTokens      int     `json:"output_tokens"`
	WallSeconds       float64 `json:"wall_seconds"`
}

func phrasesAgreement(labels string, writes []labelledWrite, results map[string]LabelResult, seed uint64, draws int) PhrasesAgreement {
	a := PhrasesAgreement{Labels: labels}
	var pGroups, rGroups, uGroups []string
	var pHits, rHits, uHits []bool
	for _, w := range writes {
		r, ok := results[w.key()]
		if !ok || r.Grade.Error != "" {
			continue
		}
		a.Writes++
		found := map[string]bool{}
		for _, id := range r.Predicted {
			found[id] = true
		}
		for _, s := range w.Sentences {
			if s.Verdict != "vacuous" && s.Verdict != "not_vacuous" {
				continue
			}
			a.Sentences++
			vacuous := s.Verdict == "vacuous"
			if vacuous {
				a.Vacuous++
				rGroups, rHits = append(rGroups, w.Group), append(rHits, found[s.ID])
			}
			if found[s.ID] {
				pGroups, pHits = append(pGroups, w.Group), append(pHits, vacuous)
				uGroups, uHits = append(uGroups, w.Group), append(uHits, vacuous)
			}
		}
		for range r.Unplaced {
			a.Unplaced++
			uGroups, uHits = append(uGroups, w.Group), append(uHits, false)
		}
		for _, c := range append([]*Call{r.Grade.Call}, refs(r.Grade.Refused)...) {
			if c != nil {
				a.CostUSD += c.CostUSD
				a.InputTokens += c.Input
				a.OutputTokens += c.Output
			}
		}
	}
	a.Precision = newShare(pGroups, pHits, draws, seed)
	a.PrecisionUnplaced = newShare(uGroups, uHits, draws, seed)
	a.Recall = newShare(rGroups, rHits, draws, seed)
	return a
}

// refs are pointers to each call of calls.
func refs(calls []Call) []*Call {
	out := make([]*Call, len(calls))
	for i := range calls {
		out[i] = &calls[i]
	}
	return out
}

func (a PhrasesAgreement) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Phrases against the labels of %s\n\n", a.Labels)
	fmt.Fprintf(&b, "Writes graded: %d. Sentences with a label: %d, of them vacuous: %d. Each interval is 95%%, a cluster bootstrap over the writes' groups.\n\n", a.Writes, a.Sentences, a.Vacuous)
	b.WriteString("| Score | Value |\n| --- | --- |\n")
	fmt.Fprintf(&b, "| precision: found sentences labelled vacuous | %s |\n", shareText(&a.Precision))
	fmt.Fprintf(&b, "| precision, a finding no sentence holds counted false | %s |\n", shareText(&a.PrecisionUnplaced))
	fmt.Fprintf(&b, "| recall: vacuous sentences found | %s |\n", shareText(&a.Recall))
	fmt.Fprintf(&b, "| findings no sentence holds | %d |\n", a.Unplaced)
	fmt.Fprintf(&b, "\nGrader cost %.2f USD, %d input and %d output tokens; wall %.0f s.\n", a.CostUSD, a.InputTokens, a.OutputTokens, a.WallSeconds)
	return b.String()
}

func runPhrasesLabels(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("writing-check harness phrases-labels", flag.ContinueOnError)
	fs.SetOutput(stderr)
	labelsDir := fs.String("labels", "", "a labeller round's directory: cases.jsonl and labels.jsonl (required)")
	out := fs.String("out", "", "the directory of the results; a run continues the writes it holds no result for (required; keep it under $TMPDIR)")
	seed := fs.Uint64("seed", DefaultSeed, "the seed of the bootstrap")
	draws := fs.Int("draws", DefaultDraws, "the bootstrap's resamples")
	mf := addModelFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(code int, err error) int {
		fmt.Fprintf(stderr, "writing-check harness phrases-labels: %v\n", err)
		return code
	}
	if *labelsDir == "" || *out == "" || fs.NArg() > 0 || *draws < 1 {
		return fail(2, errors.New("--labels and --out are required, and nothing else"))
	}
	if err := OutsideRepository("--out", *out); err != nil {
		return fail(2, err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		return fail(1, err)
	}
	writes, err := readLabelledWrites(*labelsDir)
	if err != nil {
		return fail(1, err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fail(1, err)
	}
	labels, err := filepath.Abs(*labelsDir)
	if err != nil {
		return fail(1, err)
	}
	// The results of --out are reused write by write, so a run of another grader, other prompts
	// or other labels is refused there.
	if err := checkParams(*out, phrasesRun{Grader: cfg.Models.Roles.Grader.ID, PromptsSHA256: PromptsDigest(), Labels: labels}); err != nil {
		return fail(1, err)
	}
	path := filepath.Join(*out, "results.jsonl")
	results, err := readLabelResults(path)
	if err != nil {
		return fail(1, err)
	}
	var todo []labelledWrite
	for _, w := range writes {
		if r, ok := results[w.key()]; !ok || r.Grade.Error != "" {
			todo = append(todo, w)
		}
	}
	started := time.Now()
	if len(todo) > 0 {
		runner, err := mf.runner()
		if err != nil {
			return fail(2, err)
		}
		grader, err := mf.grader(cfg, FinalInputs, runner)
		if err != nil {
			return fail(2, err)
		}
		lines, err := appendLines(path)
		if err != nil {
			return fail(1, err)
		}
		// A result its line does not hold is graded again by the next run, so the grading stops
		// at the first line that is not written.
		gradeCtx, stop := context.WithCancel(ctx)
		defer stop()
		var mu sync.Mutex
		var wg sync.WaitGroup
		feed := make(chan labelledWrite)
		done, failed := 0, 0
		for range max(mf.concurrency, 1) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for w := range feed {
					g := grader.Grade(gradeCtx, GradeInput{Judgement: JPhrases, Text: w.Text})
					if gradeCtx.Err() != nil {
						return
					}
					r := LabelResult{Key: w.key(), Family: w.Family, Group: w.Group, Grade: g, Time: time.Now().UTC().Format(time.RFC3339)}
					for _, it := range g.Items {
						if id := placeQuote(it.Quote, w.Sentences); id != "" {
							r.Predicted = append(r.Predicted, id)
						} else {
							r.Unplaced = append(r.Unplaced, it.Quote)
						}
					}
					mu.Lock()
					if err := lines.add(r); err != nil {
						stop()
					}
					done++
					if g.Error != "" {
						failed++
						fmt.Fprintf(stderr, "writing-check harness phrases-labels: %s: %s\n", r.Key, g.Error)
					}
					if done%50 == 0 || done == len(todo) {
						fmt.Fprintf(stderr, "writing-check harness phrases-labels: %d of %d writes (%d failed), %s\n", done, len(todo), failed, time.Since(started).Round(time.Second))
					}
					mu.Unlock()
				}
			}()
		}
	feeding:
		for _, w := range todo {
			select {
			case feed <- w:
			case <-gradeCtx.Done():
				break feeding
			}
		}
		close(feed)
		wg.Wait()
		if err := lines.close(); err != nil {
			return fail(1, fmt.Errorf("%w: the writes graded after it have no result, so run it again to grade them", err))
		}
		if results, err = readLabelResults(path); err != nil {
			return fail(1, err)
		}
	}
	a := phrasesAgreement(*labelsDir, writes, results, *seed, *draws)
	a.WallSeconds = time.Since(started).Seconds()
	// An agreement over some of the writes is no figure of the judgement: it is written once
	// every write has a result.
	if a.Writes < len(writes) {
		return fail(1, fmt.Errorf("%d of %d writes have no result, so no agreement is written; run it again to grade them", len(writes)-a.Writes, len(writes)))
	}
	if err := writeJSON(filepath.Join(*out, "agreement.json"), a); err != nil {
		return fail(1, err)
	}
	md := a.markdown()
	if err := os.WriteFile(filepath.Join(*out, "agreement.md"), []byte(md), 0o600); err != nil {
		return fail(1, err)
	}
	fmt.Fprint(stdout, md)
	return 0
}

// readLabelResults reads the last result of each write; a file that does not exist holds none.
func readLabelResults(path string) (map[string]LabelResult, error) {
	out := map[string]LabelResult{}
	err := eachJSONLine(path, func(line []byte) error {
		var r LabelResult
		if json.Unmarshal(line, &r) == nil && r.Key != "" {
			out[r.Key] = r
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	return out, err
}

// AskedWordsRecall is the undefined judgement against the words the owner asked about: of each
// question case's asked words that its agent text uses, the share the judgement listed.
type AskedWordsRecall struct {
	Run   string `json:"run"`
	Cases int    `json:"cases"`
	// Asked are the asked words the agent texts use; NotInText those they do not.
	Asked     int `json:"asked"`
	NotInText int `json:"not_in_text"`
	// Recall is over every asked word in the text; ShortRecall over those of at most three
	// words, which name a thing rather than quote a phrase.
	Recall      Share         `json:"recall"`
	ShortRecall Share         `json:"short_recall"`
	Words       []AskedResult `json:"words"`
}

// AskedResult is one asked word and whether the judgement listed it.
type AskedResult struct {
	CaseID string `json:"case_id"`
	Word   string `json:"word"`
	Listed bool   `json:"listed"`
}

// listedAs is whether a finding names the asked word: the words of one hold the words of the
// other, in order.
func listedAs(finding, asked string) bool {
	f, a := tokens(finding), tokens(asked)
	return within(a, f) || within(f, a)
}

func askedWordsRecall(run string, ds *Dataset, results map[string]CaseResult, seed uint64, draws int) AskedWordsRecall {
	r := AskedWordsRecall{Run: run}
	var groups, shortGroups []string
	var hits, shortHits []bool
	for _, c := range ds.Cases {
		res, ok := results[c.ID]
		g := res.Grades[JUndefined]
		if !ok || res.Error != "" || g == nil || c.Group != GroupQuestion || c.OwnerReply == nil || c.OwnerReply.Verdict == nil {
			continue
		}
		r.Cases++
		text := tokens(c.Agent.Text)
		for _, w := range c.OwnerReply.Verdict.Words {
			if !within(tokens(w), text) {
				r.NotInText++
				continue
			}
			r.Asked++
			listed := false
			for _, it := range g.Items {
				listed = listed || listedAs(it.Quote, w)
			}
			r.Words = append(r.Words, AskedResult{CaseID: c.ID, Word: w, Listed: listed})
			groups, hits = append(groups, c.SplitGroup), append(hits, listed)
			if len(tokens(w)) <= 3 {
				shortGroups, shortHits = append(shortGroups, c.SplitGroup), append(shortHits, listed)
			}
		}
	}
	r.Recall = newShare(groups, hits, draws, seed)
	r.ShortRecall = newShare(shortGroups, shortHits, draws, seed)
	return r
}

func runAskedWords(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("writing-check harness asked-words", flag.ContinueOnError)
	fs.SetOutput(stderr)
	run := fs.String("run", "", "a score run's directory whose undefined grades are measured (required)")
	casesDir := fs.String("cases", "", "the dataset build directory the run scored (required)")
	out := fs.String("out", "", "the file for the measurement as JSON (required; keep it under $TMPDIR)")
	seed := fs.Uint64("seed", DefaultSeed, "the seed of the bootstrap")
	draws := fs.Int("draws", DefaultDraws, "the bootstrap's resamples")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(code int, err error) int {
		fmt.Fprintf(stderr, "writing-check harness asked-words: %v\n", err)
		return code
	}
	if *run == "" || *casesDir == "" || *out == "" || fs.NArg() > 0 || *draws < 1 {
		return fail(2, errors.New("--run, --cases and --out are required, and nothing else"))
	}
	if err := OutsideRepository("--out", *out); err != nil {
		return fail(2, err)
	}
	var params RunParams
	data, err := os.ReadFile(filepath.Join(*run, "run.json"))
	if err == nil {
		err = json.Unmarshal(data, &params)
	}
	if err != nil {
		return fail(1, fmt.Errorf("the run's run.json: %w", err))
	}
	ds, err := LoadDataset(*casesDir, params.Part, docLimit)
	if err != nil {
		return fail(1, err)
	}
	results, err := readResults(filepath.Join(*run, "cases.jsonl"))
	if err != nil {
		return fail(1, err)
	}
	r := askedWordsRecall(*run, ds, results, *seed, *draws)
	if r.Cases == 0 {
		return fail(1, errors.New("the run holds no undefined grade of a question case"))
	}
	if err := writeJSON(*out, r); err != nil {
		return fail(1, err)
	}
	fmt.Fprintf(stdout, "# Undefined against the words the owner asked about\n\nRun %s: %d question cases; %d asked words the agent text uses (%d it does not).\n\n| Recall | Value |\n| --- | --- |\n| every asked word | %s |\n| asked words of at most three words | %s |\n",
		r.Run, r.Cases, r.Asked, r.NotInText, shareText(&r.Recall), shareText(&r.ShortRecall))
	return 0
}
