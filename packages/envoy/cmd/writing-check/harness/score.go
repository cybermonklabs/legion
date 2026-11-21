package harness

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/cmd/writing-check/harness/claude"
	"github.com/sjawhar/envoy/cmd/writing-check/harness/llm"
)

// Change is one of R3's changes: what it makes of a case's text.
type Change interface {
	// Name is the change's number, as --change names it.
	Name() string
	// Apply is the text a person sees after the change, with what it cost.
	Apply(ctx context.Context, c *Case) (Applied, error)
}

// Applied is what a change made of one case's text.
type Applied struct {
	// Text is the text a person sees after the change, and Same says it is the agent's text as
	// the agent wrote it.
	Text string
	Same bool
	// Definition is the definition Define gave, for change 6.
	Definition string
	// WriterAnswer is the writer's answer to the side question the change asked, if it asked
	// one; the added-facts judgement counts it as an input. AsksWriter says the change is one
	// that asks the writer a side question (5 or 6, alone or in a combination), so the case's
	// line keeps the answer, "" where it asked none.
	WriterAnswer string
	AsksWriter   bool
	// WaitSeconds is the time from the write, or the press, to the text a person sees.
	WaitSeconds float64
	// Calls are the change's own model calls, and Trace what each of its steps did.
	Calls []Call
	Trace []TraceStep
	// TextsFrom is the run whose case line gave all of the above (score --texts-from), "" for
	// a change this run made. AnswerNotKept says that line keeps no writer's answer, though the
	// change asks the writer a side question and the case has a session part: a line of a run
	// made before cases.jsonl kept the answer.
	TextsFrom     string
	AnswerNotKept bool
}

// asSeen is change 1: the text as the person saw it. It calls no model and makes no person wait.
type asSeen struct{}

func (asSeen) Name() string { return "1" }

func (asSeen) Apply(_ context.Context, c *Case) (Applied, error) {
	return Applied{Text: c.Agent.Text, Same: true}, nil
}

// changes are the changes the harness scores with no model of their own, by --change's value; the
// others are changeRun's (changes.go).
var changes = map[string]Change{"1": asSeen{}}

// CaseResult is one case's line of a run's cases.jsonl: what the change made of its text, the
// fresh readers' answers and every judgement.
type CaseResult struct {
	CaseID        string `json:"case_id"`
	Group         string `json:"group"`
	Kind          string `json:"kind"`
	Part          string `json:"part"`
	SplitGroup    string `json:"split_group"`
	Conversation  bool   `json:"conversation"`
	SessionStatus string `json:"session_status"`
	// Session says the case has a session part, which the judgements that read one read.
	Session       bool   `json:"session"`
	SessionSHA256 string `json:"session_sha256,omitempty"`
	Change        string `json:"change"`
	// Text is the text the person sees after the change, the string TextSHA256 hashes: an ask's
	// question with its options, one line each, as the dataset writes them; any other kind's body.
	Text       string `json:"text"`
	TextSHA256 string `json:"text_sha256"`
	Words      int    `json:"words"`
	// Definition is Define's definition (change 6).
	Definition  string      `json:"definition,omitempty"`
	WaitSeconds float64     `json:"wait_seconds"`
	ChangeCalls []Call      `json:"change_calls,omitempty"`
	ChangeTrace []TraceStep `json:"change_trace,omitempty"`
	// WriterAnswer is the writer's answer to the change's side question, which the judgements
	// read: on each line of a change that asks one (5 or 6, alone or in a combination), ""
	// where the change asked none or read none. A line of a run made before cases.jsonl kept
	// the answer has none.
	WriterAnswer *string `json:"writer_answer,omitempty"`
	// TextsFrom is the run whose line gave the text, the definition, the trace, the wait, the
	// change's calls and the writer's answer (score --texts-from): the change's calls are that
	// run's. WriterAnswerNotKept says that line keeps no writer's answer, though the change asks
	// the writer a side question and the case has a session part, and a judgement of this line
	// reads the writer's answer in its input mode: it read none.
	TextsFrom           string `json:"texts_from,omitempty"`
	WriterAnswerNotKept bool   `json:"writer_answer_not_kept,omitempty"`
	// Reader is the fresh reader's answer, and FirstParagraphReader the answer of the one who
	// read only the first paragraph of an answer case's text.
	Reader               *Answer              `json:"reader,omitempty"`
	FirstParagraphReader *Answer              `json:"first_paragraph_reader,omitempty"`
	Grades               map[Judgement]*Grade `json:"grades"`
	Error                string               `json:"error,omitempty"`
	Time                 string               `json:"time"`
}

// calls is every model call the result records as this run's: the change's, the readers' and the
// grader's. An answer or a grade the run took from another run is that run's call, not this one's;
// so is a writer's answer that another model's run of a lockstep made (Call.SharedFrom), and every
// call of a change whose text the run took from another run (TextsFrom).
func (r *CaseResult) calls() (change, measure []Call) {
	for _, c := range r.ChangeCalls {
		if c.SharedFrom == "" && r.TextsFrom == "" {
			change = append(change, c)
		}
	}
	for _, a := range []*Answer{r.Reader, r.FirstParagraphReader} {
		if a != nil && a.Call.Model != "" && !a.Reused {
			measure = append(measure, a.Call)
		}
	}
	for _, j := range Judgements {
		if g := r.Grades[j]; g != nil {
			measure = append(measure, g.ownCalls()...)
		}
	}
	return change, measure
}

// scorer scores the cases of a score process, for each of its lanes: the one run of --model, or
// each model's run of a lockstep (--models). The lanes share the fresh reader and the grader.
type scorer struct {
	ds       *Dataset
	reader   *Reader
	grader   *Grader
	sessions map[string]claude.SessionText
	places   map[string]claude.Place
	// readersFrom are the results of the run --readers-from names, whose readers' answers
	// this run grades again.
	readersFrom map[string]CaseResult
	// gradesFrom are the results of each run --grades-from names, in the flag's order, whose
	// answers and grades this run takes for a case's same text and inputs.
	gradesFrom []map[string]CaseResult
	// vocabulary is the owner's own words the undefined judgement reads (--vocabulary), and
	// readerVocabulary those the fresh reader reads (--reader-vocabulary); nil for none.
	vocabulary, readerVocabulary *Vocabulary
	// only are the judgements the run gives; empty, it gives every one.
	only []Judgement
	// skipped are the judgements the run does not give, each with the reason run.json records
	// (RunParams.SkippedJudgements): those a grader that reads no session part cannot grade.
	skipped map[Judgement]string
	// noExport says the run read no session export (--grader-no-session with no --sessions):
	// each case's session status is not_read.
	noExport bool
}

// wants is whether the run gives judgement j.
func (s *scorer) wants(j Judgement) bool {
	return (len(s.only) == 0 || slices.Contains(s.only, j)) && s.skipped[j] == ""
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// reading is one read of a case by a fresh reader: of its text (answered), or of the first
// paragraph of an answer case's text (first_paragraph). to is the result's field the answer goes
// to. taken is the answer the case takes in place of the reader's call, nil where the reader
// answers in this run: the answer a run named by --readers-from gave to this case's same text, so
// two runs that compare graders grade the same answers; else one of the answers of the runs
// --grades-from names that read the same inputs.
type reading struct {
	j     Judgement
	to    **Answer
	call  readerCall
	taken *Answer
}

// caseRun is one case as one change made its text, ready to grade: its result so far, the fresh
// readers' readings, the judgements it gets (the readings' first, each with the reader's answer
// where the case takes it), and what the runs --grades-from names hold for its same text.
type caseRun struct {
	res      CaseResult
	readings []reading
	planned  []GradeInput
	earlier  map[Judgement][]*Grade
}

// scoreCase scores case c with each of changes: the change of each lane that scores the case. It
// applies the changes one after another, so the lanes of a lockstep run that ask the writer one
// side question ask it one after another (sharedAnswers). Then it grades them all at once.
func (s *scorer) scoreCase(ctx context.Context, c *Case, changes []Change) []CaseResult {
	runs := make([]*caseRun, len(changes))
	for i, change := range changes {
		runs[i] = s.prepare(ctx, c, change)
	}
	out := make([]CaseResult, len(runs))
	var wg sync.WaitGroup
	for i, cr := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = s.grade(ctx, cr)
		}()
	}
	wg.Wait()
	return out
}

// prepare applies change to case c and lists what the case gets: the fresh readers' readings and
// every judgement the case's group takes. A change that fails leaves the result with its error,
// and no reading and no judgement.
func (s *scorer) prepare(ctx context.Context, c *Case, change Change) *caseRun {
	session := s.sessions[c.ID]
	cr := &caseRun{res: CaseResult{
		CaseID: c.ID, Group: c.Group, Kind: c.Kind, Part: c.Part, SplitGroup: c.SplitGroup,
		Conversation: c.Conversation, SessionStatus: s.places[c.ID].Status, Session: !session.Empty(),
		Change: change.Name(), Grades: map[Judgement]*Grade{},
	}}
	res := &cr.res
	if !session.Empty() {
		d := session.Digest()
		res.SessionSHA256 = hex.EncodeToString(d[:])
	}
	switch {
	case res.SessionStatus != "":
	case s.noExport:
		res.SessionStatus = "not_read"
	default:
		res.SessionStatus = "no_place"
	}
	applied, err := change.Apply(ctx, c)
	res.ChangeCalls, res.WaitSeconds, res.ChangeTrace, res.TextsFrom = applied.Calls, applied.WaitSeconds, applied.Trace, applied.TextsFrom
	if err != nil {
		res.Error = "change " + change.Name() + ": " + err.Error()
		res.Time = time.Now().UTC().Format(time.RFC3339)
		return cr
	}
	text := applied.Text
	res.Text, res.TextSHA256, res.Words, res.Definition = text, sha256Hex([]byte(text)), Words(text), applied.Definition
	if applied.AsksWriter && !applied.AnswerNotKept {
		answer := applied.WriterAnswer
		res.WriterAnswer = &answer
	}
	record := s.ds.RecordPart(c)
	question := c.PersonQuestion()
	reply := c.Group == GroupAnswer

	// Every judgement gets the case's whole context; the grader's input modes decide what each
	// call reads of it. A judgement of a text the change left as the agent wrote it, define
	// aside, reads what change 1's reads (GradeInput.asBaseline).
	base := GradeInput{Reply: reply, Question: question, Original: c.Agent.Text, Text: text, Definition: applied.Definition,
		WriterAnswer: applied.WriterAnswer, Record: record, Session: session}
	if s.wants(JUndefined) {
		base.OwnWords = s.vocabulary.knownBefore(c, text)
	}
	// earlier are what the runs --grades-from names hold for this case's same text: the readers'
	// answers and each judgement's grades. The reader and the grader take one of them where its
	// inputs were theirs, and call no model.
	var earlierReaders, earlierFirstReaders []*Answer
	for _, results := range s.gradesFrom {
		r, ok := results[c.ID]
		if !ok || r.TextSHA256 != res.TextSHA256 {
			continue
		}
		earlierReaders, earlierFirstReaders = append(earlierReaders, r.Reader), append(earlierFirstReaders, r.FirstParagraphReader)
		if cr.earlier == nil {
			cr.earlier = map[Judgement][]*Grade{}
		}
		for j, g := range r.Grades {
			cr.earlier[j] = append(cr.earlier[j], g)
		}
	}
	prior := s.readersFrom[c.ID]
	if prior.TextSHA256 != res.TextSHA256 {
		prior = CaseResult{}
	}
	// A run with no reader (a grader of another family, which grades the answers --readers-from
	// names again) calls none: a reading it holds no answer for fails the case.
	var unread []string
	read := func(j Judgement, to **Answer, done *Answer, same []*Answer, text string, firstParagraph bool) {
		r := reading{j: j, to: to}
		switch {
		case done != nil && done.Error == "":
			a := *done
			a.Reused = true
			r.taken = &a
		case s.reader == nil:
			unread = append(unread, string(j))
			return
		default:
			r.call = s.reader.call(text, question, reply || firstParagraph, firstParagraph, s.readerVocabulary.knownBefore(c, text))
			r.taken = r.call.reused(same)
		}
		cr.readings = append(cr.readings, r)
	}
	if question != "" && s.wants(JAnswered) {
		read(JAnswered, &res.Reader, prior.Reader, earlierReaders, text, false)
	}
	if reply && question != "" && s.wants(JFirstParagraph) {
		first := FirstParagraph(text)
		if first == "" {
			first = text
		}
		read(JFirstParagraph, &res.FirstParagraphReader, prior.FirstParagraphReader, earlierFirstReaders, first, true)
	}
	if len(unread) > 0 {
		res.Error = fmt.Sprintf("the run --readers-from names holds no answer of the fresh reader to this text for %s, and this run calls no reader", strings.Join(unread, ", "))
		res.Time = time.Now().UTC().Format(time.RFC3339)
		return cr
	}
	// planned are the judgements the case gets, the readings' first, each with the reader's
	// answer where the case takes it.
	give := func(j Judgement, answer string) {
		in := base
		in.Judgement, in.Answer = j, answer
		cr.planned = append(cr.planned, in)
	}
	for _, r := range cr.readings {
		answer := ""
		if r.taken != nil {
			answer = r.taken.Text
		}
		give(r.j, answer)
	}
	if reply && question != "" && s.wants(JAnswerStart) {
		give(JAnswerStart, "")
	}
	if c.Group == GroupPlain && s.wants(JHarder) {
		give(JHarder, "")
	}
	for _, j := range []Judgement{JPhrases, JUndefined} {
		if s.wants(j) {
			give(j, "")
		}
	}
	for _, j := range []Judgement{JLost, JAdded} {
		switch {
		case !s.wants(j):
		case applied.Same:
			res.Grades[j] = &Grade{ByConstruction: true}
		default:
			give(j, "")
		}
	}
	if applied.Definition != "" && s.wants(JDefine) {
		give(JDefine, "")
	}
	// A judgement that reads the writer's answer in its input mode reads none where the line of
	// --texts-from kept none: the line says so, and the figures count it.
	res.WriterAnswerNotKept = applied.AnswerNotKept && slices.ContainsFunc(cr.planned, s.grader.readsWriterAnswer)
	return cr
}

// grade makes the readings and the judgements the case plans, each at once, and ends its result.
func (s *scorer) grade(ctx context.Context, cr *caseRun) CaseResult {
	res := &cr.res
	if res.Error != "" {
		return *res
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	set := func(j Judgement, g Grade) {
		mu.Lock()
		res.Grades[j] = &g
		mu.Unlock()
	}
	run := func(f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f()
		}()
	}
	grade := func(in GradeInput) { set(in.Judgement, s.grader.GradeOrReuse(ctx, in, cr.earlier[in.Judgement])) }
	for i, r := range cr.readings {
		run(func() {
			a := r.taken
			if a == nil {
				answer := r.call.answer(ctx)
				a = &answer
			}
			mu.Lock()
			*r.to = a
			mu.Unlock()
			if a.Error == "" {
				in := cr.planned[i]
				in.Answer = a.Text
				grade(in)
			}
		})
	}
	for _, in := range cr.planned[len(cr.readings):] {
		run(func() { grade(in) })
	}
	wg.Wait()

	var errs []string
	for _, a := range []*Answer{res.Reader, res.FirstParagraphReader} {
		if a != nil && a.Error != "" {
			errs = append(errs, a.Error)
		}
	}
	for _, j := range Judgements {
		if g := res.Grades[j]; g != nil && g.Error != "" {
			errs = append(errs, string(j)+": "+g.Error)
		}
	}
	res.Error = strings.Join(errs, "; ")
	res.Time = time.Now().UTC().Format(time.RFC3339)
	return *res
}

// RunParams are what a run's figures depend on, and the pace of its own model calls. A run that
// continues in an output directory must have the parameters the directory's run.json records.
type RunParams struct {
	Change string `json:"change"`
	// Changes are the changes a combination holds, and Model the candidate the change calls; each
	// is absent for change 1.
	Changes []string `json:"changes,omitempty"`
	Model   string   `json:"model,omitempty"`
	Part    string   `json:"part"`
	Groups  []string `json:"groups"`
	// Judgements are the judgements the run gives (--judgements), absent when it gives all.
	Judgements []string `json:"judgements,omitempty"`
	// Sample is the stratified sample the run scores, absent for the whole part; under
	// --grader-no-session, a sample of the cases of the run --readers-from names. CaseListSHA256
	// is the SHA-256 of the file of the case ids the run scores (--case-list), absent without one.
	Sample         *Sample           `json:"sample,omitempty"`
	CaseListSHA256 string            `json:"case_list_sha256,omitempty"`
	Models         map[string]string `json:"models"`
	// GraderInputs are the grader's input modes (--grader-inputs, every judgement named), and
	// ReadersFrom the run whose readers' answers this run grades again.
	GraderInputs string `json:"grader_inputs"`
	ReadersFrom  string `json:"readers_from,omitempty"`
	// GraderFamily and SessionPart are a grader of another family's (--grader-no-session):
	// foundry, and none, for a grader that reads no session part; absent for a Claude grader.
	// SkippedJudgements are the judgements such a grader does not give, each with the reason.
	GraderFamily      string               `json:"grader_family,omitempty"`
	SessionPart       string               `json:"session_part,omitempty"`
	SkippedJudgements map[Judgement]string `json:"skipped_judgements,omitempty"`
	// GradesFrom are the runs whose answers and grades this run takes for the same text and
	// inputs, and VocabularySHA256 the SHA-256 of the owner's texts the undefined judgement
	// reads his own words from. ReaderVocabularySHA256 is the SHA-256 of the owner's texts the
	// fresh reader reads his own words from (--reader-vocabulary), and ReaderOwnWordsPromptSHA256
	// that of the reader's prompt part that says what they are (prompts/reader-own-words.md).
	GradesFrom                 []string `json:"grades_from,omitempty"`
	VocabularySHA256           string   `json:"vocabulary_sha256,omitempty"`
	ReaderVocabularySHA256     string   `json:"reader_vocabulary_sha256,omitempty"`
	ReaderOwnWordsPromptSHA256 string   `json:"reader_own_words_prompt_sha256,omitempty"`
	PromptsSHA256              string   `json:"prompts_sha256"`
	// AddedGrader is the added grader's variant (--added-grader), and TextsFrom the run whose
	// texts this run grades again with no call of the change (--texts-from).
	AddedGrader string `json:"added_grader"`
	TextsFrom   string `json:"texts_from,omitempty"`
	// WriterSHA256 is the writer's prompts' digest, and WriterOrder the order of the writer's
	// user message (--writer-order), for a change that plays the writer.
	WriterSHA256 string `json:"writer_sha256,omitempty"`
	WriterOrder  string `json:"writer_order,omitempty"`
	CasesSHA256  string `json:"cases_sha256"`
	SessionsSHA  string `json:"sessions_counts_sha256"`
	SessionChars int    `json:"session_chars"`
	// Rounds and Unedited are change 3's bound on its reviews and what posts with no writer
	// (--rounds, --unedited); Local the local Dispatch's settings the change reads.
	Rounds   int               `json:"rounds,omitempty"`
	Unedited string            `json:"unedited,omitempty"`
	Local    map[string]string `json:"local,omitempty"`
	// UncachedInputPerMinute is the pace of the run's own model calls in uncached input tokens a
	// minute (--uncached-input-per-minute), 0 for no pacing. So every part of a run that
	// continues in a directory was paced the same.
	UncachedInputPerMinute int `json:"uncached_input_per_minute"`
}

// readResults reads the last line of each case in a run's cases.jsonl; a line a crash cut short
// is skipped.
func readResults(path string) (map[string]CaseResult, error) {
	out := map[string]CaseResult{}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var res CaseResult
			if json.Unmarshal(line, &res) == nil && res.CaseID != "" {
				out[res.CaseID] = res
			}
		}
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// selectCases is the cases of the groups asked, in id order; with limit above 0, the first limit
// of them in the order of a SHA-256 of the seed and the case id, so a small run holds every group.
func selectCases(all []*Case, groups []string, limit int, seed uint64) []*Case {
	var out []*Case
	for _, c := range all {
		if slices.Contains(groups, c.Group) {
			out = append(out, c)
		}
	}
	if limit > 0 && limit < len(out) {
		key := func(c *Case) string { return sha256Hex([]byte(strconv.FormatUint(seed, 10) + "/" + c.ID)) }
		slices.SortFunc(out, func(a, b *Case) int { return strings.Compare(key(a), key(b)) })
		out = out[:limit]
		slices.SortFunc(out, func(a, b *Case) int { return strings.Compare(a.ID, b.ID) })
	}
	return out
}

// readCaseList reads the case ids of a --case-list file, one on each line, and answers them with
// the file's SHA-256; a blank line and a line that starts with # name no case. It refuses a file
// that names no case, or names one case twice.
func readCaseList(path string) ([]string, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var ids []string
	seen := map[string]bool{}
	for line := range strings.Lines(string(data)) {
		id := strings.TrimSpace(line)
		switch {
		case id == "" || strings.HasPrefix(id, "#"):
			continue
		case seen[id]:
			return nil, "", fmt.Errorf("%s names case %s twice", path, id)
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, "", fmt.Errorf("%s names no case", path)
	}
	return ids, sha256Hex(data), nil
}

// listCases is the cases of selected, the cases of the part and the groups the run scores, that
// ids names, in selected's order. It refuses an id that selected does not hold, so a typo cannot
// make the run smaller, and says why of each: the build holds no case of that id in the part, or
// --groups leaves out the case's group, or else, under --grader-no-session, the run
// --readers-from names holds no result for the case.
func listCases(ds *Dataset, part string, groups []string, selected []*Case, ids []string) ([]*Case, error) {
	listed := make(map[string]bool, len(ids))
	for _, id := range ids {
		listed[id] = true
	}
	var out []*Case
	for _, c := range selected {
		if listed[c.ID] {
			out = append(out, c)
			delete(listed, c.ID)
		}
	}
	var refused []string
	for _, id := range ids {
		if !listed[id] {
			continue
		}
		why := "the run --readers-from names holds no result for it"
		switch c := ds.Case(id); {
		case c == nil:
			why = "--cases holds no case of that id in the " + part + " part"
		case !slices.Contains(groups, c.Group):
			why = "its group " + c.Group + " is not one of --groups"
		}
		refused = append(refused, id+" ("+why+")")
	}
	if len(refused) > 0 {
		return nil, fmt.Errorf("--case-list names cases the run does not score: %s", strings.Join(refused, "; "))
	}
	return out, nil
}

// fileSHA256 is the SHA-256 of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeJSON writes v to path as indented JSON, by a file beside it renamed over it.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// checkParams writes params to run.json in out, or refuses a run.json of other parameters.
func checkParams[P any](out string, params P) error {
	path := filepath.Join(out, "run.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return writeJSON(path, params)
	}
	if err != nil {
		return err
	}
	var prior P
	if err := json.Unmarshal(data, &prior); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	a, _ := json.Marshal(prior)
	b, _ := json.Marshal(params)
	if string(a) != string(b) {
		return fmt.Errorf("%s holds a run of other parameters (%s); this run's are %s: give a new --out", path, a, b)
	}
	return nil
}

// foreignGraderTimeout bounds one attempt of a call of a grader of another family where
// --grader-timeout sets none, and is the least bound it may set: grok-4.7's calls had run past the
// reviewer's bound of 120 s.
const foreignGraderTimeout = 10 * time.Minute

// sessionPartNone is the session part run.json records for a grader that reads none.
const sessionPartNone = "none"

func runScore(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("writing-check harness score", flag.ContinueOnError)
	fs.SetOutput(stderr)
	changeFlag := fs.String("change", "1", "the change to score: 1 to 6, 7a, or a combination of combinations.yaml")
	modelFlag := fs.String("model", "", "the candidate of candidates.yaml the change calls, by its name: its id, or <id>:<effort> for a candidate that sets its effort (changes 3 to 7a, and a combination that holds one)")
	modelsFlag := fs.String("models", "", "in place of --model, candidates of candidates.yaml, by name as --model names one, comma-separated, that one process scores the change with in lockstep: case by case, the change with each, then the grading of all of them together; each model's run goes to <out>/<model>")
	localPath := fs.String("local", "", "the local.json of `harness local`: the local Dispatch the change calls (changes 2 to 6)")
	loadedPath := fs.String("loaded", "", "the loaded.jsonl of `harness load` into that local Dispatch")
	rounds := fs.Int("rounds", 2, "change 3: the reviews of one text, the first and those of the writer's edits; the last edit posts")
	unedited := fs.String("unedited", uneditedRewrite, "change 3 with no writer session: what posts, as DISPATCH_REVIEWER_UNEDITED says: rewrite or as_written")
	writerOrder := fs.String("writer-order", writerSessionLast, "the order of the writer's user message: session-last, Dispatch's record, the writer's text and Dispatch's message, then the session part; or session-first, the record and the session part, then the text and the message")
	casesDir := fs.String("cases", "", "the dataset build directory: cases.jsonl.gz and documents.jsonl.gz (required)")
	sessionsDir := fs.String("sessions", "", "the directory `writing-check dataset sessions` wrote: session-rows.jsonl.gz, places.jsonl, sessions-counts.json (required; optional under --grader-no-session)")
	out := fs.String("out", "", "the run's output directory, or under --models the directory of each model's run; a run continues the cases it holds no result for (required; keep it under $TMPDIR)")
	part := fs.String("part", PartDevelopment, "the part to score: development, or held_out, which the exposure log records")
	groupsFlag := fs.String("groups", strings.Join(Groups, ","), "the case groups to score, comma-separated")
	sampleSize := fs.Int("sample", 0, "score a stratified sample of about this many cases, by case group and conversation flag, each split group whole (0: the whole part)")
	caseList := fs.String("case-list", "", "a file of case ids, one on each line (a blank line and a line that starts with # are none): the run scores those cases of the part and the groups alone, and refuses an id they do not hold; not with --sample")
	sampleSeed := fs.Uint64("sample-seed", DefaultSeed, "the seed of --sample's draw")
	seed := fs.Uint64("seed", DefaultSeed, "the seed of the bootstrap")
	draws := fs.Int("draws", DefaultDraws, "the bootstrap's resamples")
	sessionChars := fs.Int("session-chars", 30000, "the bytes of a case's session, its newest, that a grading call reads")
	exposureLog := fs.String("exposure-log", DefaultExposureLog(), "the exposure log a held-out run appends to")
	commit := fs.String("commit", "", "the commit of a held-out run, for the exposure log (default: git rev-parse HEAD)")
	newRun := fs.String("new-run", "", "the reason for a second held-out run of a cell at one commit: a change, with a candidate or with none, that a run at that commit scored")
	figuresOnly := fs.Bool("figures-only", false, "compute the figures from the results the output directory holds, calling no model")
	dryRun := fs.Bool("dry-run", false, "check every flag, select the cases and write run.json as a run does, print the cases of each group and the model of each role, and call no model")
	inputsFlag := fs.String("grader-inputs", "final", "the grader's input modes: final, full or trimmed, then any ,<judgement>=full|trimmed")
	graderFlag := fs.String("grader", "", "the grading model of this run in place of candidates.yaml's roles.grader, with the same fallbacks: a Claude model with a row in prices.yaml that is no candidate and plays no other role; or, with --grader-no-session, a Foundry deployment of prices.yaml, with no fallback (default: roles.grader)")
	readerFlag := fs.String("reader", "", "the fresh reader of this run in place of candidates.yaml's roles.reader: a Claude model with a row in prices.yaml that is not the grader and no fallback grader; it may be a candidate (default: roles.reader)")
	readerVocabulary := fs.String("reader-vocabulary", "", "the owner-texts.jsonl.gz that writing-check dataset owner-texts wrote: the fresh reader also reads the words of the text it reads that the owner wrote himself before the case, in reader_own_words; the grader does not")
	noSession := fs.Bool("grader-no-session", false, "the grader reads no writer session part: required with, and only with, a --grader of another family (a Foundry deployment), which also needs --readers-from and, for a change other than 1, --texts-from, so the run calls no reader, no writer and no candidate. Each request is the one the run builds for a case with no session part; added, for the texts of a change that plays the writer, is not graded; --sessions is optional; and the cases are those of the run --readers-from names, which --sample draws from")
	graderTimeout := fs.Duration("grader-timeout", 0, "the time bound of one attempt of a grading call (0: llm's default, 180s, for a Claude grader, and 10m for a grader of another family, which takes no bound below 10m)")
	readersFrom := fs.String("readers-from", "", "a run directory whose readers' answers this run grades again, for the cases whose text is the same")
	gradesFrom := fs.String("grades-from", "", "run directories, comma-separated, whose answers and grades this run takes, calling no model, for each case whose text and whose reader's and grader's inputs are the same")
	vocabulary := fs.String("vocabulary", "", "the owner-texts.jsonl.gz that writing-check dataset owner-texts wrote: the undefined judgement reads the words of each text that the owner wrote himself before the case")
	repeatability := fs.String("repeatability", "", "a repeatability.json (harness compare) whose agreement scores.md gives beside each score")
	judgementsFlag := fs.String("judgements", "", "the judgements to give, comma-separated (default: every one), for a measurement of a few")
	addedGrader := fs.String("added-grader", AddedOld, "the added grader: old, the added judgement's request as it was; inputs-first, lost's and added's user message with the question, the record, the writer's answer and the session part first, then the original text and, last, the new text; verify, the old request, then, for a grade that lists a fact, one more call that asks of each listed fact whether an input holds it; or inputs-first-verify, lost and added as inputs-first sends them, then verify's call on the facts added lists")
	textsFrom := fs.String("texts-from", "", "a run directory, or a lockstep's directory of them, whose texts this run grades again with no call of the change: each case line's text, definition, trace, wait, change calls and writer's answer as they are; the change, its candidates, the part, the groups, the sample and --session-chars are that run's")
	mf := addModelFlags(fs)
	mf.addPaceFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	usage := func(msg string) int {
		fmt.Fprintf(stderr, "writing-check harness score: %s\n", msg)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check harness score: %v\n", err)
		return 1
	}
	// --texts-from takes from the run it names what decided the texts: their change, its
	// candidates, the part, the groups, the sample and the session part's length.
	var sources []textsSource
	textsLockstep := false
	if *textsFrom != "" {
		var given []string
		fs.Visit(func(f *flag.Flag) {
			// A grader that reads no session part draws its own sample of the cases of
			// --readers-from.
			if slices.Contains(textsFlags, f.Name) && (!*noSession || (f.Name != "sample" && f.Name != "sample-seed")) {
				given = append(given, "--"+f.Name)
			}
		})
		if len(given) > 0 {
			return usage("--texts-from takes the change, its candidates, the part, the groups, the sample and the session part's length from the run it names: give no " + strings.Join(given, ", "))
		}
		var err error
		if sources, textsLockstep, err = readTextsFrom(*textsFrom); err != nil {
			return usage(err.Error())
		}
		p := sources[0].params
		*changeFlag, *part, *groupsFlag, *sessionChars = p.Change, p.Part, strings.Join(p.Groups, ","), p.SessionChars
		if p.Sample != nil && !*noSession {
			*sampleSize, *sampleSeed = p.Sample.Size, p.Sample.Seed
		}
	}
	change, plan, err := resolveChange(*changeFlag)
	if err != nil {
		return usage(err.Error())
	}
	var lockstep []string
	if *modelsFlag != "" {
		for _, m := range strings.Split(*modelsFlag, ",") {
			lockstep = append(lockstep, strings.TrimSpace(m))
		}
	}
	candidateSet := *modelFlag != "" || len(lockstep) > 0
	groups := strings.Split(*groupsFlag, ",")
	groupsSet := false
	fs.Visit(func(f *flag.Flag) { groupsSet = groupsSet || f.Name == "groups" })
	if plan != nil && plan.has(changeDefine) && !groupsSet {
		groups = []string{GroupQuestion}
	}
	// A run of --texts-from makes no change: its plan is the texts' run's, and the run plays no
	// step of it.
	textsPlan := plan
	if len(sources) > 0 {
		plan = nil
	}
	switch {
	case *casesDir == "" || (*sessionsDir == "" && !*noSession) || *out == "" || fs.NArg() > 0:
		return usage("--cases, --sessions (optional under --grader-no-session) and --out are required, and nothing else")
	case *graderTimeout < 0:
		return usage("--grader-timeout must not be below 0")
	case *part != PartDevelopment && *part != PartHeldOut:
		return usage("--part is development or held_out")
	case slices.ContainsFunc(groups, func(g string) bool { return !slices.Contains(Groups, g) }):
		return usage("--groups names a group that is not one of " + strings.Join(Groups, ", "))
	case plan != nil && plan.has(changeDefine) && !slices.Equal(groups, []string{GroupQuestion}):
		return usage("Define (change 6) defines the words of the person's question: it scores the question cases alone (--groups question)")
	case *judgementsFlag != "" && slices.ContainsFunc(strings.Split(*judgementsFlag, ","), func(j string) bool { return !slices.Contains(Judgements, Judgement(j)) }):
		return usage("--judgements names a judgement that is not one of the judgements of R2")
	case *draws < 1 || *sessionChars < 1 || *sampleSize < 0:
		return usage("--draws and --session-chars must be above 0, and --sample not below 0")
	case *part != PartHeldOut && (*newRun != "" || *commit != ""):
		return usage("--commit and --new-run are for a held-out run")
	case *modelFlag != "" && len(lockstep) > 0:
		return usage("give --model or --models, not both")
	case slices.Contains(lockstep, "") || len(slices.Compact(slices.Sorted(slices.Values(lockstep)))) != len(lockstep):
		return usage("--models names an empty model, or one model twice")
	case plan == nil && (candidateSet || *localPath != "" || *loadedPath != ""):
		return usage("change 1 calls no model and reads no local Dispatch: give no --model, --models, --local or --loaded")
	case plan != nil && plan.candidate() != candidateSet:
		return usage(fmt.Sprintf("change %s: --model or --models names the candidate of the reviewer, Rewrite, Define and the detector's reader, which the change calls exactly when it holds 3, 4, 5, 6 or 7a", change.Name()))
	case plan != nil && plan.local() && (*localPath == "" || *loadedPath == ""):
		return usage(fmt.Sprintf("change %s calls the local Dispatch: give --local and --loaded", change.Name()))
	case *rounds < 1 || (*unedited != uneditedRewrite && *unedited != uneditedAsWritten):
		return usage("--rounds must be at least 1, and --unedited rewrite or as_written")
	case *writerOrder != writerSessionLast && *writerOrder != writerSessionFirst:
		return usage("--writer-order is session-last or session-first")
	case *writerOrder != writerSessionLast && (plan == nil || !plan.writer()):
		return usage(fmt.Sprintf("change %s plays no writer: give no --writer-order", change.Name()))
	case mf.uncachedPerMinute < 0:
		return usage("--uncached-input-per-minute must not be below 0")
	case !slices.Contains(AddedGraders, *addedGrader):
		return usage("--added-grader is " + strings.Join(AddedGraders, ", "))
	case len(sources) > 0 && *part != PartDevelopment:
		return usage("--texts-from takes a run of the development part: grading the held-out texts again is a second run of their cells")
	case *caseList != "" && *sampleSize > 0:
		return usage("give --case-list or --sample, not both (a run of --texts-from takes the sample of the run it names)")
	case *readersFrom != "" && (*readerFlag != "" || *readerVocabulary != ""):
		return usage("--readers-from takes the fresh reader's answers of another run, which --reader and --reader-vocabulary do not decide: give one or the other")
	case *dryRun && *figuresOnly:
		return usage("give --dry-run or --figures-only, not both")
	}
	if err := OutsideRepository("--out", *out); err != nil {
		return usage(err.Error())
	}
	inputs, err := ParseGraderInputs(*inputsFlag)
	if err != nil {
		return usage(err.Error())
	}
	var priorReaders map[string]CaseResult
	if *readersFrom != "" {
		if priorReaders, err = readResults(filepath.Join(*readersFrom, "cases.jsonl")); err != nil || len(priorReaders) == 0 {
			return usage(fmt.Sprintf("--readers-from %s holds no results (%v)", *readersFrom, err))
		}
	}
	var priorGrades []map[string]CaseResult
	var gradesDirs []string
	for _, dir := range strings.Split(*gradesFrom, ",") {
		if dir = strings.TrimSpace(dir); dir == "" {
			continue
		}
		results, err := readResults(filepath.Join(dir, "cases.jsonl"))
		if err != nil || len(results) == 0 {
			return usage(fmt.Sprintf("--grades-from %s holds no results (%v)", dir, err))
		}
		priorGrades, gradesDirs = append(priorGrades, results), append(gradesDirs, dir)
	}
	var ownWords *Vocabulary
	var vocabularySHA string
	if *vocabulary != "" {
		if ownWords, err = ReadVocabulary(*vocabulary); err != nil {
			return usage(fmt.Sprintf("--vocabulary: %v", err))
		}
		if vocabularySHA, err = fileSHA256(*vocabulary); err != nil {
			return fail(err)
		}
	}
	var readerWords *Vocabulary
	var readerVocabularySHA string
	switch {
	case *readerVocabulary == "":
	case *readerVocabulary == *vocabulary:
		readerWords, readerVocabularySHA = ownWords, vocabularySHA
	default:
		if readerWords, err = ReadVocabulary(*readerVocabulary); err != nil {
			return usage(fmt.Sprintf("--reader-vocabulary: %v", err))
		}
		if readerVocabularySHA, err = fileSHA256(*readerVocabulary); err != nil {
			return fail(err)
		}
	}
	var caseIDs []string
	var caseListSHA string
	if *caseList != "" {
		if caseIDs, caseListSHA, err = readCaseList(*caseList); err != nil {
			return usage(fmt.Sprintf("--case-list: %v", err))
		}
	}
	var agreement *Repeatability
	if *repeatability != "" {
		if agreement, err = readRepeatability(*repeatability); err != nil {
			return usage(err.Error())
		}
	}
	cfg, err := LoadConfig()
	if err != nil {
		return fail(err)
	}
	// A grader of another family reads no session part, and grades again what another run's
	// reader answered and its change wrote, so the run makes that model's grading calls alone,
	// none of which carries session text (R2).
	foreign := cfg.Prices.Prices[*graderFlag].Family == llm.FamilyFoundry
	switch {
	case foreign && !*noSession:
		return usage(fmt.Sprintf("--grader %q is a Foundry deployment, which session text never reaches (R2): give --grader-no-session", *graderFlag))
	case *noSession && !foreign:
		return usage("--grader-no-session is for a --grader of another family, a Foundry deployment of prices.yaml: a Claude grader reads the session part as candidates.yaml's grader does")
	case foreign && *readersFrom == "":
		return usage(fmt.Sprintf("--grader %q grades again the fresh reader's answers of another run, so its run calls no reader: give --readers-from", *graderFlag))
	case foreign && plan != nil:
		return usage(fmt.Sprintf("--grader %q grades again the texts of another run, so its run calls no writer and no candidate: give --texts-from for change %s", *graderFlag, change.Name()))
	case foreign && *part != PartDevelopment:
		return usage(fmt.Sprintf("--grader %q grades again runs of the development part alone: a held-out run is graded by candidates.yaml's grader", *graderFlag))
	case foreign && *graderTimeout != 0 && *graderTimeout < foreignGraderTimeout:
		return usage(fmt.Sprintf("--grader-timeout for a grader of another family is at least %s", foreignGraderTimeout))
	}
	// The run's reader first, so --grader is checked against the reader the run calls.
	if cfg, err = cfg.WithReader(*readerFlag); err != nil {
		return usage(err.Error())
	}
	var skipped map[Judgement]string
	if foreign {
		cfg = cfg.WithNoSessionGrader(*graderFlag)
		for _, src := range sources {
			if m, ok := cfg.Candidate(src.params.Model); ok && m.ID == *graderFlag {
				return usage(fmt.Sprintf("--grader %q wrote the texts of --texts-from %s: a grader grades no output it wrote (R2)", *graderFlag, src.dir))
			}
		}
		// added counts the facts of the new text that its inputs do not hold. A change that
		// plays the writer wrote its texts with the writer's session, an input that this grader
		// does not read, so it cannot tell a fact of the session from an added one.
		if textsPlan != nil && textsPlan.writer() {
			skipped = map[Judgement]string{JAdded: fmt.Sprintf("change %s plays the writer, whose session part is an input of the new text, and the grader reads no session part", change.Name())}
		}
	} else if cfg, err = cfg.WithGrader(*graderFlag, false); err != nil {
		return usage(err.Error())
	}
	started := time.Now()
	casesSHA, err := fileSHA256(filepath.Join(*casesDir, casesFile))
	if err != nil {
		return fail(err)
	}
	// A run of a grader that reads no session part needs no session export; with none, its
	// run.json records no sessions_counts_sha256.
	sessionsSHA := ""
	if *sessionsDir != "" {
		if sessionsSHA, err = fileSHA256(filepath.Join(*sessionsDir, "sessions-counts.json")); err != nil {
			return fail(err)
		}
	}
	if len(sources) > 0 {
		if p := sources[0].params; p.CasesSHA256 != casesSHA || (*sessionsDir != "" && p.SessionsSHA != sessionsSHA) {
			return usage(fmt.Sprintf("--texts-from %s scored other cases or sessions than --cases and --sessions hold: its run.json records cases_sha256 %s and sessions_counts_sha256 %s", *textsFrom, p.CasesSHA256, p.SessionsSHA))
		}
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fail(err)
	}
	var fallbacks []string
	for _, f := range cfg.Models.Roles.GraderFallbacks {
		fallbacks = append(fallbacks, f.ID)
	}
	models := map[string]string{RoleGrader: cfg.Models.Roles.Grader.ID, RoleGraderFallback: strings.Join(fallbacks, ","), RoleReader: cfg.Models.Roles.Reader.ID}
	// candidates are the candidate of each lane: the models of --models, in its order, or the one
	// of --model, "" for a change that calls none; under --texts-from, the candidate of each run
	// it names.
	candidates, candidateFlag := lockstep, "--models"
	if len(lockstep) == 0 {
		candidates, candidateFlag = []string{*modelFlag}, "--model"
	}
	if len(sources) > 0 {
		candidates = nil
		for _, src := range sources {
			candidates = append(candidates, src.params.Model)
		}
		if textsLockstep {
			lockstep = candidates
		}
	}
	var run *changeRun
	var inst LocalInstance
	if plan != nil {
		run = change.(*changeRun)
		run.cfg, run.rounds, run.unedited, run.tries, run.backoff, run.poll = cfg, *rounds, *unedited, changeTries, changeBackoff, changePoll
		run.writerOrder = *writerOrder
		if plan.writer() {
			models[RoleWriter] = cfg.Models.Roles.Writer.ID
		}
		for _, m := range candidates {
			if _, ok := cfg.Candidate(m); m != "" && !ok {
				return usage(fmt.Sprintf("%s %q is no candidate of candidates.yaml", candidateFlag, m))
			}
		}
		if plan.local() {
			if inst, err = readLocalInstance(*localPath); err != nil {
				return usage(err.Error())
			}
			if run.loaded, err = readLoaded(*loadedPath, inst.records()); err != nil {
				return usage(err.Error())
			}
			if run.rules, err = instanceRules(inst, *plan); err != nil {
				return usage(err.Error())
			}
		}
	}
	ds, err := LoadDataset(*casesDir, *part, docLimit)
	if err != nil {
		return fail(err)
	}
	selected := selectCases(ds.Cases, groups, 0, 0)
	if foreign {
		// A run that calls no reader grades the cases of the run --readers-from names.
		selected = slices.DeleteFunc(selected, func(c *Case) bool { _, ok := priorReaders[c.ID]; return !ok })
	}
	if caseIDs != nil {
		if selected, err = listCases(ds, *part, groups, selected, caseIDs); err != nil {
			return usage(err.Error())
		}
	}
	params := RunParams{
		Change: change.Name(), Part: *part, Groups: groups, GraderInputs: inputs.String(), ReadersFrom: *readersFrom,
		GradesFrom: gradesDirs, VocabularySHA256: vocabularySHA, ReaderVocabularySHA256: readerVocabularySHA, AddedGrader: *addedGrader,
		PromptsSHA256: PromptsDigest(), CasesSHA256: casesSHA, SessionsSHA: sessionsSHA, SessionChars: *sessionChars,
		CaseListSHA256: caseListSHA, UncachedInputPerMinute: mf.uncachedPerMinute,
	}
	if readerWords != nil {
		params.ReaderOwnWordsPromptSHA256 = ReaderOwnWordsDigest()
	}
	if foreign {
		params.GraderFamily, params.SessionPart, params.SkippedJudgements = llm.FamilyFoundry, sessionPartNone, skipped
	}
	if plan != nil {
		if len(plan.changes()) > 1 || plan.name != plan.steps[0] {
			params.Changes = plan.changes()
		}
		if plan.writer() {
			params.WriterSHA256, params.WriterOrder = WriterDigest(), *writerOrder
		}
		if plan.has(changeFindings) {
			params.Rounds, params.Unedited = *rounds, *unedited
		}
		if plan.local() {
			params.Local = inst.readSettings()
		}
	}
	var only []Judgement
	if *judgementsFlag != "" {
		for _, j := range Judgements {
			if slices.Contains(strings.Split(*judgementsFlag, ","), string(j)) {
				only = append(only, j)
				params.Judgements = append(params.Judgements, string(j))
			}
		}
	}
	if *sampleSize > 0 {
		var sample Sample
		selected, sample = drawSample(selected, *sampleSize, *sampleSeed)
		params.Sample = &sample
	}
	if len(sources) > 0 && !foreign {
		drawn, _ := json.Marshal(params.Sample)
		recorded, _ := json.Marshal(sources[0].params.Sample)
		if string(drawn) != string(recorded) {
			return fail(fmt.Errorf("the sample drawn from --cases, %s, is not the one --texts-from %s records, %s", drawn, *textsFrom, recorded))
		}
	}
	// Each lane's run.json is the one a run of --model with its candidate writes, so either
	// command continues the other's directory.
	lanes := make([]*lane, len(candidates))
	for i, m := range candidates {
		l := &lane{change: change, model: m, out: *out, params: params}
		if len(lockstep) > 0 {
			l.out = filepath.Join(*out, m)
			if err := os.MkdirAll(l.out, 0o755); err != nil {
				return fail(err)
			}
		}
		l.params.Models = maps.Clone(models)
		if m != "" {
			l.params.Model, l.params.Models[RoleCandidate] = m, m
		}
		if len(sources) > 0 {
			l.change = sources[i].change(textsPlan)
			sources[i].keep(&l.params)
		}
		if err := checkParams(l.out, l.params); err != nil {
			return fail(err)
		}
		if l.done, err = readResults(filepath.Join(l.out, "cases.jsonl")); err != nil {
			return fail(err)
		}
		lanes[i] = l
	}
	// open counts the lanes' cases with no line in cases.jsonl, and retry those whose line holds
	// an error: this run scores both.
	open, retry := 0, 0
	for _, l := range lanes {
		for _, c := range selected {
			switch r, ok := l.done[c.ID]; {
			case !ok:
				open++
			case r.Error != "":
				retry++
			}
		}
	}
	// A held-out run is checked against the exposure log here, so a run the log refuses calls no
	// model, and is claimed in the log just before it scores a case of the part (below): no model
	// has read one before that line, so a run that stops after it, for any reason, still counts as
	// one that read the part, and a run that stops before it on a refusal of its own read none.
	// A dry run calls no model, so the log does not hold it.
	var heldOut *Exposure
	finished := open+retry == 0
	if *part == PartHeldOut && !*figuresOnly && !*dryRun {
		c := *commit
		if c == "" {
			if c, err = headCommit(); err != nil {
				return fail(fmt.Errorf("the commit of a held-out run: %w; give --commit", err))
			}
		}
		changed := []string{change.Name()}
		if plan != nil {
			changed = plan.changes()
		}
		logged := maps.Clone(models)
		if candidateSet {
			logged[RoleCandidate] = strings.Join(candidates, ",")
		}
		dir, err := filepath.Abs(*out)
		if err != nil {
			return fail(err)
		}
		e := Exposure{Commit: c, Time: started.UTC().Format(time.RFC3339), Changes: changed, Models: logged, Reason: *newRun, Out: dir, Retried: retry}
		if _, err := checkHeldOut(*exposureLog, e, finished); err != nil {
			return fail(err)
		}
		heldOut = &e
	}
	var todo []*Case
	for _, l := range lanes {
		l.todo = map[string]bool{}
		for _, c := range selected {
			if r, ok := l.done[c.ID]; !ok || r.Error != "" {
				l.todo[c.ID] = true
			}
		}
		with := ""
		if len(lockstep) > 0 {
			with = " with " + l.model
		}
		if l.params.TextsFrom != "" {
			with += ", the texts of " + l.params.TextsFrom
		}
		fmt.Fprintf(stderr, "writing-check harness score: change %s%s, part %s: %d cases, %d with a result, %d to score\n",
			change.Name(), with, *part, len(selected), len(selected)-len(l.todo), len(l.todo))
	}
	for _, c := range selected {
		if slices.ContainsFunc(lanes, func(l *lane) bool { return l.todo[c.ID] }) {
			todo = append(todo, c)
		}
	}
	if *dryRun {
		// The run's Claude clients call no model when they are built, so a dry run builds them
		// and refuses what they refuse. A Foundry grader's client signs in, so it builds none.
		runner, err := mf.runner()
		if err != nil {
			return usage(err.Error())
		}
		if !foreign {
			if _, err := mf.grader(cfg, inputs, runner); err != nil {
				return usage(err.Error())
			}
			if _, err := mf.claudeClient(cfg, cfg.Models.Roles.Reader, runner); err != nil {
				return usage(err.Error())
			}
		}
		printDryRun(stdout, groups, selected, lanes)
		return 0
	}
	ids := make([]string, len(selected))
	for i, c := range selected {
		ids[i] = c.ID
	}
	var places map[string]claude.Place
	if *sessionsDir != "" {
		if places, err = claude.ReadPlaces(*sessionsDir); err != nil {
			return fail(err)
		}
	}
	var uncached llm.PaceUse
	if !*figuresOnly && len(todo) > 0 {
		// A grader that reads no session part reads no session export: the run holds no
		// session text.
		var sessions map[string]claude.SessionText
		if !foreign {
			if sessions, err = claude.CachedExcerpts(*sessionsDir, ids, *sessionChars, filepath.Join(*out, "session-excerpts.jsonl.gz")); err != nil {
				return fail(fmt.Errorf("the session parts: %w", err))
			}
		}
		runner, err := mf.runner()
		if err != nil {
			return usage(err.Error())
		}
		// The grader's calls share the run's bound and pacer, with their own time bound.
		graderRunner, bound := runner, *graderTimeout
		if bound == 0 && foreign {
			bound = foreignGraderTimeout
		}
		if bound != 0 {
			own := *runner
			own.Timeout = bound
			graderRunner = &own
		}
		grader, err := mf.grader(cfg, inputs, graderRunner)
		if err != nil {
			return usage(err.Error())
		}
		grader.Added = *addedGrader
		// A grader of another family grades the answers of --readers-from: the run has no
		// reader.
		var reader *Reader
		if !foreign {
			readerModel, err := mf.claudeClient(cfg, cfg.Models.Roles.Reader, runner)
			if err != nil {
				return usage(err.Error())
			}
			reader = &Reader{Model: readerModel}
		}
		if run != nil {
			if err := run.connect(ctx, mf, inst, runner, sessions, ds, selected); err != nil {
				return fail(err)
			}
			runs, err := run.withCandidates(mf, candidates, len(lockstep) > 0)
			if err != nil {
				return fail(err)
			}
			for i, l := range lanes {
				l.change = runs[i]
			}
		}
		if heldOut != nil {
			e, err := ClaimHeldOut(*exposureLog, *heldOut, finished)
			if err != nil {
				return fail(err)
			}
			if e.Continues != "" {
				fmt.Fprintf(stderr, "writing-check harness score: the exposure log %s records this run as a continuation of the held-out run begun %s at %s\n", *exposureLog, e.Continues, e.Commit)
			} else {
				fmt.Fprintf(stderr, "writing-check harness score: the exposure log %s records this held-out run at %s\n", *exposureLog, e.Commit)
			}
		}
		s := &scorer{ds: ds, reader: reader, grader: grader, sessions: sessions, places: places, readersFrom: priorReaders,
			gradesFrom: priorGrades, vocabulary: ownWords, readerVocabulary: readerWords, only: only, skipped: skipped, noExport: *sessionsDir == ""}
		if err := scoreAll(ctx, s, lanes, todo, mf.concurrency, stderr, started); err != nil {
			return fail(err)
		}
		uncached = runner.Pacer.Use()
		for _, l := range lanes {
			if l.done, err = readResults(filepath.Join(l.out, "cases.jsonl")); err != nil {
				return fail(err)
			}
		}
	}
	// Each lane's figures are a run of --model's: the same function of its results. The wall time
	// and the uncached input are the process's, which a lockstep's runs share.
	var missing []string
	for _, l := range lanes {
		var results []CaseResult
		for _, c := range selected {
			if r, ok := l.done[c.ID]; ok && r.Error == "" {
				results = append(results, r)
			}
		}
		figures := computeFigures(l.params, results, len(selected), *seed, *draws, cfg)
		figures.Run.WallSeconds = time.Since(started).Seconds()
		figures.Run.ScoredThisRun, figures.Run.FailedThisRun = l.scored, l.failed
		figures.Run.UncachedInput = uncached
		figures.Run.Lockstep = lockstep
		figures.Repeatability = agreement
		if err := writeJSON(filepath.Join(l.out, "scores.json"), figures); err != nil {
			return fail(err)
		}
		md := figures.markdown()
		if err := os.WriteFile(filepath.Join(l.out, "scores.md"), []byte(md), 0o600); err != nil {
			return fail(err)
		}
		fmt.Fprint(stdout, md)
		if n := len(selected) - len(results); n > 0 {
			of := ""
			if len(lockstep) > 0 {
				of = " of " + l.model
			}
			missing = append(missing, fmt.Sprintf("%d of %d cases%s", n, len(selected), of))
		}
	}
	if ctx.Err() != nil {
		return fail(fmt.Errorf("stopped: %w; run it again to continue", ctx.Err()))
	}
	if len(missing) > 0 {
		return fail(fmt.Errorf("%s have no result; run it again to score them", strings.Join(missing, ", ")))
	}
	return 0
}

// lane is one candidate's run in a score process: the change with that candidate, its output
// directory with the parameters its run.json records, the results its cases.jsonl holds, the
// cases this process scores of it (todo), and how many it scored and how many of those ended with
// an error. A run of --model, or of a change with no candidate, has one lane, whose directory is
// --out; a lockstep run (--models) has one for each model, in the flag's order, in <out>/<model>.
type lane struct {
	change         Change
	model          string
	out            string
	params         RunParams
	done           map[string]CaseResult
	todo           map[string]bool
	scored, failed int
}

// printDryRun prints what score --dry-run checked: the cases the run selected, in all and in each
// of its groups; the model of each role as run.json records it, the candidate of each lane among
// them; and each run.json it wrote.
func printDryRun(w io.Writer, groups []string, selected []*Case, lanes []*lane) {
	counts := map[string]int{}
	for _, c := range selected {
		counts[c.Group]++
	}
	by := make([]string, len(groups))
	for i, g := range groups {
		by[i] = fmt.Sprintf("%s %d", g, counts[g])
	}
	fmt.Fprintf(w, "dry run, no model called: %d cases (%s)\n", len(selected), strings.Join(by, ", "))
	models := lanes[0].params.Models
	for _, role := range []string{RoleReader, RoleGrader, RoleGraderFallback, RoleWriter} {
		switch m, ok := models[role]; {
		case m != "":
			fmt.Fprintf(w, "%s: %s\n", role, m)
		case ok:
			fmt.Fprintf(w, "%s: none\n", role)
		}
	}
	var candidates []string
	for _, l := range lanes {
		if l.model != "" {
			candidates = append(candidates, l.model)
		}
	}
	if len(candidates) > 0 {
		fmt.Fprintf(w, "%s: %s\n", RoleCandidate, strings.Join(candidates, ","))
	}
	for _, l := range lanes {
		fmt.Fprintf(w, "run.json: %s\n", filepath.Join(l.out, "run.json"))
	}
}

// scoreAll scores each of cases for the lanes whose todo holds it, workers cases at once
// (scorer.scoreCase), and appends each lane's result to the lane's cases.jsonl as the case ends.
// A result its line does not hold is scored again by the next run, so the run stops at the first
// line that is not written, and answers it; else it answers a file's failure to close.
func scoreAll(ctx context.Context, s *scorer, lanes []*lane, cases []*Case, workers int, progress io.Writer, started time.Time) error {
	files := make([]*lineFile, 0, len(lanes))
	closeAll := func() error {
		var first error
		for _, f := range files {
			if err := f.close(); err != nil && first == nil {
				first = err
			}
		}
		return first
	}
	for _, l := range lanes {
		f, err := appendLines(filepath.Join(l.out, "cases.jsonl"))
		if err != nil {
			closeAll()
			return err
		}
		files = append(files, f)
	}
	scoreCtx, stop := context.WithCancel(ctx)
	defer stop()
	var mu sync.Mutex
	ended, failed := 0, 0
	feed := make(chan *Case)
	var wg sync.WaitGroup
	for range max(workers, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range feed {
				var in []int
				var changes []Change
				for i, l := range lanes {
					if l.todo[c.ID] {
						in, changes = append(in, i), append(changes, l.change)
					}
				}
				results := s.scoreCase(scoreCtx, c, changes)
				if scoreCtx.Err() != nil {
					return
				}
				mu.Lock()
				for k, res := range results {
					l := lanes[in[k]]
					if err := files[in[k]].add(res); err != nil {
						stop()
					}
					l.scored++
					if res.Error != "" {
						l.failed++
						failed++
						model := ""
						if len(lanes) > 1 {
							model = "model " + l.model + ", "
						}
						fmt.Fprintf(progress, "writing-check harness score: %scase %s: %s\n", model, res.CaseID, res.Error)
					}
				}
				ended++
				if ended%50 == 0 || ended == len(cases) {
					fmt.Fprintf(progress, "writing-check harness score: %d of %d cases (%d failed), %s\n", ended, len(cases), failed, time.Since(started).Round(time.Second))
				}
				mu.Unlock()
			}
		}()
	}
feeding:
	for _, c := range cases {
		select {
		case feed <- c:
		case <-scoreCtx.Done():
			break feeding
		}
	}
	close(feed)
	wg.Wait()
	if err := closeAll(); err != nil {
		return fmt.Errorf("%w: the cases scored after it have no result, so run it again to score them", err)
	}
	return nil
}
