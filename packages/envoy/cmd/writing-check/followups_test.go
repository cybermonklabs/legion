package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/cmd/writing-check/harness"
	"github.com/sjawhar/envoy/internal/dispatch/wordrecords"
)

// followupsFixture is one issue for each thing followups reads: a question the agent explains and
// the owner's reply to the explanation, with a note of his on the issue after it; a question no
// agent text replies to; two questions one explanation answers; an ask he answers by an option
// alone; a thread another person opened, where he replies to the agent and later to the thread's
// head; and a question to the agent, whose answer he replies to.
func followupsFixture() *dsFixture {
	f := newDSFixture()
	other := map[string]any{"kind": "user", "id": "other@example.com"}
	f.issue("FUP-1", "", "Relay", "The relay spec.")
	f.comment("a1", "FUP-1", fixtureAgent, "The relay drains the queue before each window.", "")
	f.owner("q1", "FUP-1", "What is the relay?", "a1", wordrecords.WordMeaning)
	f.comment("e1", "FUP-1", fixtureAgent, "The relay is the job that copies events out of the log.", "q1")
	f.owner("n1", "FUP-1", "Clear, thanks.", "e1", wordrecords.None)
	f.owner("note1", "FUP-1", "Ship the relay this week.", "", wordrecords.None)

	f.issue("FUP-2", "", "Window", "The window spec.")
	f.comment("a2", "FUP-2", fixtureAgent, "The window closes at the cutover.", "")
	f.owner("q2", "FUP-2", "I cannot follow this.", "a2", wordrecords.CannotUnderstand)

	f.issue("FUP-3", "", "Mirror", "The mirror spec.")
	f.comment("a3", "FUP-3", fixtureAgent, "The mirror lags behind the primary by the fan-out.", "")
	f.owner("q3a", "FUP-3", "Too much jargon here.", "a3", wordrecords.JargonComplaint)
	f.owner("q3b", "FUP-3", "What is the fan-out?", "a3", wordrecords.WordMeaning)
	f.comment("e3", "FUP-3", fixtureAgent, "The fan-out is how many copies the mirror writes at once.", "q3b")

	f.issue("FUP-4", "", "Order", "The order spec.")
	f.event("ask.opened", fixtureAgent, "FUP-4", map[string]any{"id": "ask-f", "question": "Which mirror goes first?", "options": []map[string]any{{"label": "East"}, {"label": "West"}}})
	f.event("ask.answered", fixturePerson, "FUP-4", map[string]any{"id": "ask-f", "answer": map[string]any{"selected": []string{"East"}}})

	f.issue("FUP-5", "", "Backups", "The backup spec.")
	f.comment("p1", "FUP-5", other, "Where do the backups go?", "")
	f.comment("p2", "FUP-5", fixtureAgent, "The nightly job copies the backups to the east bucket.", "p1")
	f.owner("r2", "FUP-5", "Fine, go ahead.", "p2", wordrecords.None)
	f.owner("r3", "FUP-5", "Also keep a copy in the west bucket.", "p1", wordrecords.None)

	f.issue("FUP-6", "", "Build", "The build spec.")
	f.owner("o6", "FUP-6", "Why did the build stop?", "", wordrecords.AgentQuestion)
	f.comment("r6", "FUP-6", fixtureAgent, "It stopped because the cache was full.", "o6")
	f.owner("ok6", "FUP-6", "Understood.", "r6", wordrecords.None)
	return f
}

// nextKeys is the keys and the places of a followup's next texts.
func nextKeys(f dsFollowup) []string {
	var out []string
	for _, n := range f.Next {
		out = append(out, n.Key+" "+n.Where)
	}
	return out
}

// followups, run on the bundle build read and the cases it wrote, finds each question case's
// explanation and the owner's reply to it, leaves out an owner text written before the subject,
// lists an answer by an option alone as an act, and finds a plain case's later reply in its
// thread. The explanations are a dataset harness reads as answer cases.
func TestFollowupsReadWhatTheOwnerDidNext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	in := dsInputs{Seed: fixtureSeed}
	in.Snapshot, in.Verdicts = followupsFixture().write(t, dir)
	build := filepath.Join(dir, "build")
	if _, err := buildDataset(in, build); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "bundle.tar.gz")
	if _, _, err := packBundle(in, bundle, dsPartSize); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "followups")
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"dataset", "followups", "--from", bundle, "--cases", filepath.Join(build, "cases.jsonl.gz"), "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("followups: exit %d: %s", code, stderr.String())
	}
	byAgent := map[string]dsFollowup{}
	groups := map[string]int{}
	for _, f := range readGzipJSONL[dsFollowup](t, filepath.Join(out, dsFollowupsFile)) {
		groups[f.Group]++
		key := f.Group + " "
		if f.Question != nil {
			key += f.Question.Key
		} else {
			key += f.Subject.Key
		}
		byAgent[key] = f
		if f.Subject == nil {
			continue
		}
		for _, n := range f.Next {
			if n.EventID <= f.Subject.EventID || n.SecondsAfter <= 0 {
				t.Errorf("case %s lists %s, written at event %d, %.0f s after its subject at event %d", f.CaseID, n.Key, n.EventID, n.SecondsAfter, f.Subject.EventID)
			}
		}
	}
	if groups[dsGroupQuestion] != 4 || groups[dsGroupAnswer] != 1 || groups[dsGroupPlain] != 3 || groups[dsGroupPhrase] != 0 {
		t.Fatalf("followups by group %v, want 4 question, 1 answer and 3 plain cases", groups)
	}

	// The question's explanation is the agent's reply to it, and the owner's reply to the
	// explanation comes first; his question, written before it, is not among the next texts.
	q1 := byAgent["question comment:q1"]
	if q1.Subject == nil || q1.Subject.Key != "comment:e1" || q1.SubjectRole != dsSubjectExplanation || q1.SubjectFound != dsFoundDirectReply ||
		q1.AskedAbout == nil || q1.AskedAbout.Key != "comment:a1" || q1.Question.Text != "What is the relay?" {
		t.Fatalf("question q1's followup = %+v, want the explanation e1 of the question about a1", q1)
	}
	if got := nextKeys(q1); !slices.Equal(got, []string{"comment:n1 reply_chain", "comment:note1 issue"}) {
		t.Errorf("q1's next texts = %v, want his reply to the explanation, then his note on the issue", got)
	}
	if n := q1.Next[0]; !n.RepliesToSubject || n.Verdict != wordrecords.None || n.Text != "Clear, thanks." || q1.Next[1].RepliesToSubject {
		t.Errorf("q1's next texts = %+v, want the reply to the subject with its verdict, and the note not replying to it", q1.Next)
	}
	if q1.AgentTextsBeforeNext == nil || *q1.AgentTextsBeforeNext != 0 {
		t.Errorf("q1: agent texts before the next text = %v, want 0", q1.AgentTextsBeforeNext)
	}
	// A question no agent text replies to has no subject.
	if q2 := byAgent["question comment:q2"]; q2.Subject != nil || len(q2.Next) != 0 || q2.AgentTextsBeforeNext != nil {
		t.Errorf("question q2's followup = %+v, want no subject", q2)
	}
	// One explanation answers two questions: by the thread for the first, directly for the second.
	q3a, q3b := byAgent["question comment:q3a"], byAgent["question comment:q3b"]
	if q3a.Subject == nil || q3a.Subject.Key != "comment:e3" || q3a.SubjectFound != dsFoundThread ||
		q3b.Subject == nil || q3b.Subject.Key != "comment:e3" || q3b.SubjectFound != dsFoundDirectReply {
		t.Errorf("questions q3a and q3b have subjects %+v (%s) and %+v (%s), want e3 for both", q3a.Subject, q3a.SubjectFound, q3b.Subject, q3b.SubjectFound)
	}
	// An answer by an option alone, after the ask, is an act.
	ask := byAgent["plain ask:ask-f"]
	if len(ask.Next) != 0 || len(ask.Acts) != 1 || ask.Acts[0].EventType != "ask.answered" || ask.Acts[0].Target != "ask:ask-f" ||
		!slices.Equal(ask.Acts[0].Selected, []string{"East"}) {
		t.Errorf("the ask's followup = %+v, want no next text and the answer that chose East", ask)
	}
	// A plain case's later reply in its thread follows the reply to it.
	if got := nextKeys(byAgent["plain comment:p2"]); !slices.Equal(got, []string{"comment:r2 reply_chain", "comment:r3 thread"}) {
		t.Errorf("p2's next texts = %v, want his reply to it, then his later reply in its thread", got)
	}
	if got := nextKeys(byAgent["answer comment:r6"]); !slices.Equal(got, []string{"comment:ok6 reply_chain"}) {
		t.Errorf("the answer case's next texts = %v, want his reply to the answer", got)
	}

	// The explanations: one answer case for each explanation, with the question cases it explains,
	// the owner's question and his first next text.
	explanations := readGzipJSONL[dsExplanationCase](t, filepath.Join(out, dsExplanationsDir, "cases.jsonl.gz"))
	if len(explanations) != 2 {
		t.Fatalf("explanations = %+v, want e1 and e3", explanations)
	}
	e1, e3 := explanations[0], explanations[1]
	if e1.Agent.Key != "comment:e1" || e1.Group != dsGroupAnswer || !strings.HasPrefix(e1.ID, "answer/comment:e1@") ||
		!slices.Equal(e1.OfQuestion, []string{q1.CaseID}) || e1.Question == nil || e1.Question.Key != "comment:q1" ||
		e1.Question.Verdict == nil || e1.Question.Verdict.Verdict != wordrecords.WordMeaning ||
		e1.OwnerReply == nil || e1.OwnerReply.Key != "comment:n1" || e1.OwnerReply.Verdict == nil ||
		e1.Tree != q1.Tree || e1.Part != q1.Part || e1.SplitGroup != q1.SplitGroup || e1.Found != dsFoundDirectReply ||
		e1.WriterSession != fixtureAgent["id"] || len(e1.Record.Thread) != 2 || len(e1.Record.Documents) != 1 {
		t.Errorf("explanation e1 = %+v, want the answer case of q1's explanation", e1)
	}
	if e3.Agent.Key != "comment:e3" || !slices.Equal(e3.OfQuestion, []string{q3a.CaseID, q3b.CaseID}) || e3.OwnerReply != nil {
		t.Errorf("explanation e3 = %+v, want one case of both questions, with no owner reply", e3)
	}
	ds, err := harness.LoadDataset(filepath.Join(out, dsExplanationsDir), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if c := ds.Case(e1.ID); c == nil || c.PersonQuestion() != "What is the relay?" || c.Record.Documents[0].Missing {
		t.Errorf("harness reads explanation e1 as %+v", c)
	}
	var counts dsExplanationCounts
	data, err := os.ReadFile(filepath.Join(out, dsExplanationsDir, "counts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &counts); err != nil {
		t.Fatal(err)
	}
	if counts.Owner != fixtureOwner || counts.QuestionCases != 4 || counts.Explained != 3 || counts.NoExplanation != 1 || counts.Cases != 2 ||
		counts.OfSeveral != 1 || counts.WithOwnerReply != 1 || counts.SessionsListed != 1 || counts.SHA256["sessions.tsv"] == "" {
		t.Errorf("explanations' counts = %+v", counts)
	}
	for _, want := range []string{"| question | ", "explanations written: 2 answer cases (1 explain more than one question case, 1 hold an owner reply)",
		"with none (no agent text replies to the question): 1", dsFollowupsFile + " sha256: "} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("followups printed:\n%s\nwant %q", stdout.String(), want)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(out, ".bundle-*")); len(left) != 0 {
		t.Errorf("followups left %v", left)
	}
	if code := run(t.Context(), []string{"dataset", "followups", "--from", bundle, "--out", out}, &stdout, &stderr); code != 2 {
		t.Errorf("followups with no --cases: exit %d, want 2", code)
	}
}
