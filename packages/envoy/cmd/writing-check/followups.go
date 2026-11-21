package main

// followups (dataset followups) writes what the owner did after the text each case reads: his own
// texts written after it and his acts that wrote no text. It is the ground truth of INFRA-3231's
// dataset check: did he understand the agent's text? For a question case the text is the agent's
// explanation, its first reply to his question; followups also writes those explanations as a
// dataset of answer cases in build's format, so harness score grades each one as it grades an
// answer case. It reads the bundle build reads and the cases build wrote, and calls no model.
// README.md, "followups", says what it writes.

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sjawhar/envoy/cmd/writing-check/harness"
	"github.com/sjawhar/envoy/internal/dispatch/wordrecords"
)

const (
	dsFollowupsFile   = "followups.jsonl.gz"
	dsExplanationsDir = "explanations"
	// dsNextTexts is the most owner texts a followup lists, and dsNextChars where each is cut.
	dsNextTexts = 5
	dsNextChars = 4000
	// dsIssueWindow is how long after the subject an owner text on its issue still follows it.
	dsIssueWindow = 72 * time.Hour
)

// The roles of a followup's subject: the agent's first reply to a question case's question, or
// the case's own agent text.
const (
	dsSubjectExplanation = "explanation"
	dsSubjectAgentText   = "agent_text"
)

// dsFollowupGroups is the case groups followups reads, in the order it prints them.
var dsFollowupGroups = []string{dsGroupQuestion, dsGroupAnswer, dsGroupPlain}

// dsFollowup is one line of followups.jsonl.gz.
type dsFollowup struct {
	CaseID     string `json:"case_id"`
	Group      string `json:"group"`
	Part       string `json:"part"`
	SplitGroup string `json:"split_group"`
	Tree       string `json:"tree"`
	// Subject is the text whose reception the line reads; nil for a question case whose question
	// no agent text replies to. SubjectFound is how firstReply found an explanation.
	Subject      *dsCaseText         `json:"subject"`
	SubjectRole  string              `json:"subject_role"`
	SubjectFound string              `json:"subject_found,omitempty"`
	Question     *dsFollowupQuestion `json:"question,omitempty"`
	AskedAbout   *dsTextRef          `json:"asked_about,omitempty"`
	Next         []dsNextText        `json:"next"`
	Acts         []dsFollowupAct     `json:"acts"`
	// AgentTextsBeforeNext is how many agent texts the subject's thread, or its conversation for
	// a message on no issue, holds between the subject and the first next text; nil with none.
	AgentTextsBeforeNext *int `json:"agent_texts_before_next"`
}

// dsTextRef names a text by its key and the event that wrote it, as a case names its texts.
type dsTextRef struct {
	Key     string `json:"key"`
	EventID int64  `json:"event_id"`
}

// dsFollowupQuestion is the owner's question of a question case.
type dsFollowupQuestion struct {
	Key     string `json:"key"`
	EventID int64  `json:"event_id"`
	Time    string `json:"time"`
	Text    string `json:"text"`
}

// dsNextText is one text of the owner written after the subject, with the build's own verdict of
// it.
type dsNextText struct {
	Key              string   `json:"key"`
	EventID          int64    `json:"event_id"`
	Time             string   `json:"time"`
	Kind             string   `json:"kind"`
	Text             string   `json:"text"`
	Verdict          string   `json:"verdict"`
	Words            []string `json:"words"`
	RepliesToSubject bool     `json:"replies_to_subject"`
	Where            string   `json:"where"`
	SecondsAfter     float64  `json:"seconds_after"`
}

// dsFollowupAct is one act of the owner that wrote no text, after the subject.
type dsFollowupAct struct {
	EventID   int64    `json:"event_id"`
	EventType string   `json:"event_type"`
	Time      string   `json:"time"`
	Target    string   `json:"target"`
	Selected  []string `json:"selected,omitempty"`
}

// dsExplanationCase is one case of explanations/cases.jsonl.gz: an answer case as build writes
// one, and the question cases whose explanation its agent text is.
type dsExplanationCase struct {
	dsCase
	OfQuestion []string `json:"of_question"`
}

// dsFollowupRow is the counts of one case group's cases in one part.
type dsFollowupRow struct {
	Cases        int `json:"cases"`
	SubjectFound int `json:"subject_found"`
	WithNext     int `json:"with_next"`
	WithAct      int `json:"with_act"`
	WithNeither  int `json:"with_neither"`
}

// dsExplanationCounts is explanations/counts.json. It holds no text.
type dsExplanationCounts struct {
	End              string            `json:"end"`
	Owner            string            `json:"owner"`
	NamingIssue      string            `json:"naming_issue"`
	Seed             uint64            `json:"seed"`
	FromCasesSHA256  string            `json:"from_cases_sha256"`
	QuestionCases    int               `json:"question_cases"`
	Explained        int               `json:"question_cases_with_explanation"`
	NoExplanation    int               `json:"question_cases_with_no_explanation"`
	NotACaseText     int               `json:"question_cases_whose_explanation_is_no_case_text"`
	Cases            int               `json:"cases"`
	OfSeveral        int               `json:"cases_of_several_questions"`
	WithOwnerReply   int               `json:"cases_with_owner_reply"`
	Parts            map[string]int    `json:"parts"`
	CasesWithSession int               `json:"cases_with_session"`
	SessionsListed   int               `json:"sessions_listed"`
	SessionsUnlisted int               `json:"sessions_not_a_store_id"`
	SHA256           map[string]string `json:"sha256"`
}

// dsFollowupsResult is what followups wrote and counted.
type dsFollowupsResult struct {
	rows         map[string]map[string]*dsFollowupRow // by case group, then part
	explanations dsExplanationCounts
	followupsSHA string
}

func (r *dsFollowupsResult) row(group, part string) *dsFollowupRow {
	if r.rows[group] == nil {
		r.rows[group] = map[string]*dsFollowupRow{}
	}
	if r.rows[group][part] == nil {
		r.rows[group][part] = &dsFollowupRow{}
	}
	return r.rows[group][part]
}

func runDatasetFollowups(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check dataset followups", flag.ContinueOnError)
	flags.SetOutput(stderr)
	from := flags.String("from", "", "the bundle build read: the tarball, its manifest, or a directory holding one (required)")
	casesPath := flags.String("cases", "", "the cases build wrote from that bundle: the build directory or its cases.jsonl.gz (required)")
	out := flags.String("out", "", "the directory for followups.jsonl.gz and explanations/ (required; keep it under $TMPDIR)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *from == "" || *casesPath == "" || *out == "" || flags.NArg() > 0 {
		fmt.Fprintln(stderr, "writing-check dataset followups: --from, --cases and --out are required, and nothing else")
		return 2
	}
	if err := harness.OutsideRepository("--out", *out); err != nil {
		fmt.Fprintf(stderr, "writing-check dataset followups: %v\n", err)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "writing-check dataset followups: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fail(err)
	}
	dir, err := os.MkdirTemp(*out, ".bundle-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(dir)
	in, err := openBundle(*from, dir)
	if err != nil {
		return fail(err)
	}
	res, err := writeFollowups(in, *casesPath, *out)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "| Group | Part | Cases | Subject found | With a next text | With an act | With neither |\n| --- | --- | ---: | ---: | ---: | ---: | ---: |\n")
	for _, g := range dsFollowupGroups {
		for _, p := range []string{dsDevelopment, dsHeldOut} {
			r := res.row(g, p)
			fmt.Fprintf(stdout, "| %s | %s | %d | %d | %d | %d | %d |\n", g, p, r.Cases, r.SubjectFound, r.WithNext, r.WithAct, r.WithNeither)
		}
	}
	e := res.explanations
	fmt.Fprintf(stdout, "question cases: %d; with an explanation: %d; with none (no agent text replies to the question): %d; whose explanation is no case text: %d\n",
		e.QuestionCases, e.Explained, e.NoExplanation, e.NotACaseText)
	fmt.Fprintf(stdout, "explanations written: %d answer cases (%d explain more than one question case, %d hold an owner reply)\n", e.Cases, e.OfSeveral, e.WithOwnerReply)
	fmt.Fprintf(stdout, "%s sha256: %s\n", dsFollowupsFile, res.followupsSHA)
	for _, name := range []string{"cases.jsonl.gz", "documents.jsonl.gz", "sessions.tsv"} {
		fmt.Fprintf(stdout, "%s/%s sha256: %s\n", dsExplanationsDir, name, e.SHA256[name])
	}
	return 0
}

// dsFollowupIndex is the index and what followups looks up beside it: the owner's own texts with
// their times, by issue and by conversation, each broadcast's later copies, and the verdicts.
type dsFollowupIndex struct {
	ix       *dsIndex
	verdicts map[string]dsVerdict
	owner    map[*dsText]time.Time
	byIssue  map[string][]*dsText
	byConv   map[string][]*dsText
	copies   map[*dsText][]*dsText
}

func newDSFollowupIndex(ix *dsIndex, verdicts map[string]dsVerdict) (*dsFollowupIndex, error) {
	fx := &dsFollowupIndex{ix: ix, verdicts: verdicts, owner: map[*dsText]time.Time{}, byIssue: map[string][]*dsText{},
		byConv: map[string][]*dsText{}, copies: map[*dsText][]*dsText{}}
	for _, t := range ix.ownerTexts() {
		at, err := time.Parse(time.RFC3339Nano, t.Time)
		if err != nil {
			return nil, fmt.Errorf("owner text %s: %w", t.tid(), err)
		}
		fx.owner[t] = at
		if t.Issue != "" {
			fx.byIssue[t.Issue] = append(fx.byIssue[t.Issue], t)
		}
		if t.Conv != "" {
			fx.byConv[t.Conv] = append(fx.byConv[t.Conv], t)
		}
	}
	for _, c := range ix.copies {
		fx.copies[c.copyOf] = append(fx.copies[c.copyOf], c)
	}
	return fx, nil
}

// exact is the text of key that event wrote, nil when the snapshot holds none. A broadcast's
// later copy is no case's text.
func (ix *dsIndex) exact(ref dsTextRef) *dsText {
	for _, t := range ix.texts[ref.Key] {
		if t.EventID == ref.EventID && t.copyOf == nil {
			return t
		}
	}
	return nil
}

// dsPick is an owner text written after a subject, and where it was found.
type dsPick struct {
	text  *dsText
	where string
}

// byEvent orders texts by event, then by key.
func byEvent(a, b *dsText) int {
	return cmp.Or(cmp.Compare(a.EventID, b.EventID), strings.Compare(a.Key, b.Key))
}

// next is the owner's own texts written after s, at most dsNextTexts: first those whose reply
// chain reaches s, then the rest of its thread, then, for a message on no issue, the rest of its
// conversation, then those on its issue written within dsIssueWindow after it; each part in event
// order.
func (fx *dsFollowupIndex) next(s *dsText, at time.Time) []dsPick {
	var out []dsPick
	picked := map[*dsText]bool{}
	add := func(list []*dsText, where string, keep func(*dsText) bool) {
		for _, t := range list {
			if len(out) == dsNextTexts {
				return
			}
			if _, mine := fx.owner[t]; !mine || t.EventID <= s.EventID || picked[t] || keep != nil && !keep(t) {
				continue
			}
			picked[t] = true
			out = append(out, dsPick{text: t, where: where})
		}
	}
	add(fx.replies(s), dsFoundReplyChain, nil)
	add(fx.thread(s), dsFoundThread, nil)
	if s.Conv != "" {
		add(fx.byConv[s.Conv], dsFoundConversation, nil)
	}
	if s.Issue != "" {
		list := fx.byIssue[s.Issue]
		i := sort.Search(len(list), func(i int) bool { return list[i].EventID > s.EventID })
		limit := at.Add(dsIssueWindow)
		add(list[i:], dsFoundIssue, func(t *dsText) bool { return !fx.owner[t].After(limit) })
	}
	return out
}

// replies is every text whose reply chain reaches s, or a later copy of s, in event order.
func (fx *dsFollowupIndex) replies(s *dsText) []*dsText {
	var out []*dsText
	stack := []string{s.Key}
	for _, c := range fx.copies[s] {
		stack = append(stack, c.Key)
	}
	seen := map[string]bool{}
	for len(stack) > 0 {
		key := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[key] {
			continue
		}
		seen[key] = true
		for _, c := range fx.ix.children[key] {
			out = append(out, c)
			stack = append(stack, c.Key)
		}
	}
	slices.SortFunc(out, byEvent)
	return out
}

// thread is the texts of s's thread, and of the threads of its later copies, in event order.
func (fx *dsFollowupIndex) thread(s *dsText) []*dsText {
	var own []*dsText
	if s.Thread != "" {
		own = fx.ix.threadTexts[s.Thread]
	}
	if len(fx.copies[s]) == 0 {
		return own
	}
	out := slices.Clone(own)
	for _, c := range fx.copies[s] {
		out = append(out, fx.ix.threadTexts[c.Thread]...)
	}
	slices.SortFunc(out, byEvent)
	return out
}

// agentsBefore is how many agent texts s's thread, or its conversation for a message on no
// issue, holds written after s and before first.
func (fx *dsFollowupIndex) agentsBefore(s, first *dsText) int {
	list := fx.ix.threadAgents[cmp.Or(s.Thread, s.Key)]
	if s.Conv != "" {
		list = fx.ix.convAgents[s.Conv]
	}
	n := 0
	for _, a := range list {
		if a.EventID > s.EventID && a.EventID < first.EventID && a.usable() {
			n++
		}
	}
	return n
}

// acts is the owner's acts that wrote no text, on s or on a text of its thread, after s.
func (fx *dsFollowupIndex) acts(s *dsText) []dsFollowupAct {
	out := []dsFollowupAct{}
	acts := fx.ix.acts
	for _, a := range acts[sort.Search(len(acts), func(i int) bool { return acts[i].eventID > s.EventID }):] {
		if a.target == s.Key || s.Thread != "" && fx.ix.threadOf(a.target) == s.Thread {
			out = append(out, dsFollowupAct{EventID: a.eventID, EventType: a.eventType, Time: a.time, Target: a.target, Selected: a.selected})
		}
	}
	return out
}

// nextText is a pick as a followup holds it, its text cut at dsNextChars, with the build's own
// verdict of it and how long after the subject's time at it was written.
func (fx *dsFollowupIndex) nextText(at time.Time, p dsPick) dsNextText {
	t := p.text
	v := fx.verdicts[t.tid()]
	return dsNextText{Key: t.Key, EventID: t.EventID, Time: t.Time, Kind: t.Kind, Text: wordrecords.Clip(t.Text, dsNextChars),
		Verdict: v.Verdict, Words: nonNil(v.Words), RepliesToSubject: p.where == dsFoundReplyChain, Where: p.where,
		SecondsAfter: fx.owner[t].Sub(at).Seconds()}
}

// caseVerdict is a verdict as a case holds it.
func caseVerdict(v dsVerdict) *dsCaseVerdict {
	return &dsCaseVerdict{Verdict: v.Verdict, Words: nonNil(v.Words), Model: v.Model, PromptSHA256: v.PromptSHA256}
}

// nonNil is list, or an empty list where it is nil, so its JSON is [] and not null.
func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// dsCaseLine is the part of a case followups reads.
type dsCaseLine struct {
	ID         string     `json:"id"`
	Group      string     `json:"group"`
	Part       string     `json:"part"`
	SplitGroup string     `json:"split_group"`
	Tree       string     `json:"tree"`
	Agent      dsTextRef  `json:"agent_text"`
	OwnerReply *dsTextRef `json:"owner_reply"`
}

// writeFollowups writes followups.jsonl.gz and the explanations' dataset into out, from the
// bundle's inputs and the cases build wrote from them, and answers its counts. It loads the
// index as build does, and refuses a case whose texts the snapshot does not hold.
func writeFollowups(in dsInputs, casesPath, out string) (dsFollowupsResult, error) {
	res := dsFollowupsResult{rows: map[string]map[string]*dsFollowupRow{}}
	// The cases first: a wrong --cases fails before the snapshot is read.
	casesPath = dsCasesFile(casesPath)
	casesSHA, err := fileSHA256(casesPath)
	if err != nil {
		return res, err
	}
	var cases []dsCaseLine
	err = eachGzipLine(casesPath, func(line []byte) error {
		var c dsCaseLine
		if err := json.Unmarshal(line, &c); err != nil {
			return err
		}
		if slices.Contains(dsFollowupGroups, c.Group) {
			cases = append(cases, c)
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	slices.SortFunc(cases, func(a, b dsCaseLine) int { return strings.Compare(a.ID, b.ID) })

	ix, err := loadIndex(in.Snapshot, nil)
	if err != nil {
		return res, err
	}
	verdicts, _, err := readVerdicts(in.Verdicts)
	if err != nil {
		return res, err
	}
	// The document versions among the candidates hold their text, as build reads them.
	if err := ix.loadCandidateVersions(in.Snapshot); err != nil {
		return res, err
	}
	fx, err := newDSFollowupIndex(ix, verdicts)
	if err != nil {
		return res, err
	}

	ec := dsExplanationCounts{End: ix.end, Owner: ix.owner, NamingIssue: ix.namingIssue, Seed: in.Seed, FromCasesSHA256: casesSHA,
		Parts: map[string]int{}, SHA256: map[string]string{}}
	ownerText := func(t *dsText) *dsCaseText {
		ct := ix.caseText(t)
		if v, ok := verdicts[t.tid()]; ok {
			ct.Verdict = caseVerdict(v)
		}
		return &ct
	}
	explained := map[*dsText]*dsExplanationCase{}
	var explanations []*dsExplanationCase
	lines := make([]dsFollowup, 0, len(cases))
	for _, c := range cases {
		f := dsFollowup{CaseID: c.ID, Group: c.Group, Part: c.Part, SplitGroup: c.SplitGroup, Tree: c.Tree,
			SubjectRole: dsSubjectAgentText, Next: []dsNextText{}, Acts: []dsFollowupAct{}}
		agent := ix.exact(c.Agent)
		if agent == nil {
			return res, fmt.Errorf("case %s: the snapshot holds no text %s@%d: the cases were built from another bundle", c.ID, c.Agent.Key, c.Agent.EventID)
		}
		subject := agent
		var question *dsText
		if c.Group == dsGroupQuestion {
			if c.OwnerReply != nil {
				question = ix.exact(*c.OwnerReply)
			}
			if question == nil {
				return res, fmt.Errorf("case %s: the snapshot holds no owner question of it: the cases were built from another bundle", c.ID)
			}
			ec.QuestionCases++
			f.SubjectRole = dsSubjectExplanation
			f.Question = &dsFollowupQuestion{Key: question.Key, EventID: question.EventID, Time: question.Time, Text: question.Text}
			f.AskedAbout = &dsTextRef{Key: agent.Key, EventID: agent.EventID}
			subject, f.SubjectFound = ix.firstReply(question)
		}
		row := res.row(c.Group, c.Part)
		row.Cases++
		var next []dsPick
		if subject != nil {
			at, err := time.Parse(time.RFC3339Nano, subject.Time)
			if err != nil {
				return res, fmt.Errorf("case %s: %w", c.ID, err)
			}
			ct := ix.caseText(subject)
			f.Subject = &ct
			next = fx.next(subject, at)
			for _, p := range next {
				f.Next = append(f.Next, fx.nextText(at, p))
			}
			f.Acts = fx.acts(subject)
			if len(next) > 0 {
				n := fx.agentsBefore(subject, next[0].text)
				f.AgentTextsBeforeNext = &n
			}
			row.SubjectFound++
		}
		if len(f.Next) > 0 {
			row.WithNext++
		}
		if len(f.Acts) > 0 {
			row.WithAct++
		}
		if len(f.Next) == 0 && len(f.Acts) == 0 {
			row.WithNeither++
		}
		lines = append(lines, f)

		// The explanation, as an answer case of the question case's tree and part.
		switch {
		case question == nil:
		case subject == nil:
			ec.NoExplanation++
		case !subject.caseKind():
			ec.NotACaseText++
		case explained[subject] != nil:
			ec.Explained++
			e := explained[subject]
			e.OfQuestion = append(e.OfQuestion, c.ID)
		default:
			ec.Explained++
			e := &dsExplanationCase{dsCase: dsCase{ID: dsGroupAnswer + "/" + subject.tid(), Group: dsGroupAnswer, Found: f.SubjectFound, agent: subject},
				OfQuestion: []string{c.ID}}
			ix.fill(&e.dsCase)
			e.Tree, e.SplitGroup, e.Part = c.Tree, c.SplitGroup, c.Part
			e.Question = ownerText(question)
			if len(next) > 0 {
				e.OwnerReply = ownerText(next[0].text)
			}
			explained[subject] = e
			explanations = append(explanations, e)
		}
	}
	if res.followupsSHA, err = dsWriteGzipJSONL(filepath.Join(out, dsFollowupsFile), lines); err != nil {
		return res, err
	}

	dir := filepath.Join(out, dsExplanationsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	slices.SortFunc(explanations, func(a, b *dsExplanationCase) int { return strings.Compare(a.ID, b.ID) })
	if ec.SHA256["cases.jsonl.gz"], err = dsWriteGzipJSONL(filepath.Join(dir, "cases.jsonl.gz"), explanations); err != nil {
		return res, err
	}
	asCases := make([]*dsCase, len(explanations))
	for i, e := range explanations {
		asCases[i] = &e.dsCase
		ec.Parts[e.Part]++
		if len(e.OfQuestion) > 1 {
			ec.OfSeveral++
		}
		if e.OwnerReply != nil {
			ec.WithOwnerReply++
		}
		if e.WriterSession != "" {
			ec.CasesWithSession++
		}
	}
	docs := dsRecordDocuments(asCases)
	if ec.SHA256["documents.jsonl.gz"], err = ix.writeDocuments(in.Snapshot, filepath.Join(dir, "documents.jsonl.gz"), docs); err != nil {
		return res, err
	}
	if ec.SHA256["sessions.tsv"], ec.SessionsListed, ec.SessionsUnlisted, err = writeSessionList(filepath.Join(dir, "sessions.tsv"), asCases); err != nil {
		return res, err
	}
	ec.Cases = len(explanations)
	ec.SHA256[dsFollowupsFile] = res.followupsSHA
	if err := writeJSON(filepath.Join(dir, "counts.json"), ec); err != nil {
		return res, err
	}
	res.explanations = ec
	return res, nil
}
