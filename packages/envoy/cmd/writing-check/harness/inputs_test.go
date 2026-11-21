package harness

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

func TestParseGraderInputs(t *testing.T) {
	gi, err := ParseGraderInputs("trimmed,answered=full")
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range Judgements {
		want := InputsTrimmed
		if j == JAnswered {
			want = InputsFull
		}
		if gi[j] != want {
			t.Errorf("%s = %q, want %q", j, gi[j], want)
		}
	}
	if all, _ := ParseGraderInputs("full"); gi.String() != "trimmed,answered=full" || all.String() != InputsFull {
		t.Errorf("String = %q", gi.String())
	}
	final, err := ParseGraderInputs("final")
	if err != nil || final.String() != FinalInputs.String() {
		t.Errorf("final = %v (%v), want %s", final, err, FinalInputs.String())
	}
	for _, bad := range []string{"some", "trimmed,answered", "trimmed,answered=half", "full,nojudgement=full"} {
		if _, err := ParseGraderInputs(bad); err == nil {
			t.Errorf("%q was read", bad)
		}
	}
	// A judgement whose subject is the question reads it in either mode.
	if c := (GraderInputs{JAnswerStart: InputsTrimmed}).context(JAnswerStart); !c.Question || c.Record || c.Session {
		t.Errorf("answer_start trimmed reads %+v", c)
	}
	// answered reads the text beside the reader's answer in either mode; first_paragraph, whose
	// reader read only the first paragraph, does not.
	for _, mode := range []string{InputsTrimmed, InputsFull} {
		gi := GraderInputs{JAnswered: mode, JFirstParagraph: mode}
		if !gi.context(JAnswered).Text || gi.context(JFirstParagraph).Text {
			t.Errorf("%s: answered reads %+v, first_paragraph %+v", mode, gi.context(JAnswered), gi.context(JFirstParagraph))
		}
	}
}

// requestsOf are the texts of the requests whose prompt holds marker.
func requestsOf(stand *modeltest.Model, marker string) []string {
	var out []string
	for _, r := range stand.Requests() {
		if strings.Contains(r.Text, marker) {
			out = append(out, r.Text)
		}
	}
	return out
}

func TestGraderInputsDecideWhatACallReads(t *testing.T) {
	build, sessions := writeFixture(t)
	score := func(inputs string) (*modeltest.Model, map[string]CaseResult) {
		stand := modeltest.New(t, graderRules(t)...)
		out := t.TempDir()
		code, _, stderr := runHarness("score", "--cases", build, "--sessions", sessions, "--out", out, "--grader-inputs", inputs,
			"--gateway", stand.URL, "--gateway-key-file", keyFile(t), "--draws", "50", "--exposure-log", filepath.Join(t.TempDir(), "exposure.log"))
		if code != 0 {
			t.Fatalf("score --grader-inputs %s = %d: %s", inputs, code, stderr)
		}
		results, err := readResults(filepath.Join(out, "cases.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return stand, results
	}

	stand, results := score("full")
	q := results["question/comment:q1@11"]
	for _, j := range []Judgement{JAnswered, JUndefined} {
		if g := q.Grades[j]; g == nil || g.Inputs != InputsFull || !g.Session {
			t.Errorf("full: the question case's %s grade: %+v", j, g)
		}
	}
	for _, text := range requestsOf(stand, markUndefined) {
		if strings.Contains(text, "Please build the zork train.") && (!strings.Contains(text, "<record>") || !strings.Contains(text, "<writer_session>")) {
			t.Error("full: the question case's undefined call does not read the record and the session")
		}
	}
	// The phrases judgement reads its candidate sentences alone, in either mode.
	for _, text := range requestsOf(stand, markPhrases) {
		if strings.Contains(text, "<record>") || strings.Contains(text, "<writer_session>") {
			t.Error("full: a phrases call read the record or the session")
		}
	}

	stand, results = score("trimmed")
	q = results["question/comment:q1@11"]
	for _, j := range []Judgement{JAnswered, JUndefined} {
		if g := q.Grades[j]; g == nil || g.Inputs != InputsTrimmed || g.Session {
			t.Errorf("trimmed: the question case's %s grade: %+v", j, g)
		}
	}
	for _, marker := range []string{markAnswered, markPhrases, markUndefined, markStart, markHarder} {
		for _, text := range requestsOf(stand, marker) {
			if strings.Contains(text, "<writer_session>") {
				t.Errorf("trimmed: a call of %q read the session", marker)
			}
		}
	}
	for _, text := range requestsOf(stand, markHarder) {
		if strings.Contains(text, "<record>") || strings.Contains(text, `"record"`) {
			t.Error("trimmed: harder read the record")
		}
	}
	for _, text := range requestsOf(stand, markUndefined) {
		if !strings.Contains(text, "<record>") {
			t.Error("trimmed: undefined did not read the record")
		}
	}
}

// refusing is a gateway in front of stand that answers each request whose text holds a marker of
// refuse and whose model is one of that marker's models with a refusal, as Anthropic's Messages
// API stops a reply.
func refusing(t *testing.T, stand *modeltest.Model, refuse map[string][]string) string {
	t.Helper()
	target, err := url.Parse(stand.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var wire struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &wire)
		refused := false
		for marker, models := range refuse {
			refused = refused || (slices.Contains(models, wire.Model) && bytes.Contains(body, []byte(marker)))
		}
		if !refused {
			proxy.ServeHTTP(w, r)
			return
		}
		usage := map[string]any{"input_tokens": 10, "output_tokens": 0}
		if !wire.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg_refused", "type": "message", "role": "assistant", "model": wire.Model,
				"content": []any{}, "stop_reason": "refusal", "stop_sequence": nil, "usage": usage})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(name string, v any) {
			data, _ := json.Marshal(v)
			_, _ = io.WriteString(w, "event: "+name+"\ndata: "+string(data)+"\n\n")
		}
		send("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_refused", "type": "message", "role": "assistant",
			"model": wire.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": usage}})
		send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "refusal", "stop_sequence": nil}, "usage": usage})
		send("message_stop", map[string]any{"type": "message_stop"})
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestAJudgementTheGraderRefusesIsGradedByAFallback(t *testing.T) {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	fallbacks := cfg.Models.Roles.GraderFallbacks
	if len(fallbacks) < 2 {
		t.Fatalf("candidates.yaml names %d fallback graders, want at least 2", len(fallbacks))
	}
	grader, first, second := cfg.Models.Roles.Grader.ID, fallbacks[0].ID, fallbacks[1].ID
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, graderRules(t)...)
	// The grader refuses phrases and undefined; the first fallback refuses undefined too.
	gateway := refusing(t, stand, map[string][]string{markPhrases: {grader}, markUndefined: {grader, first}})
	out := t.TempDir()
	code, _, stderr := runHarness("score", "--cases", build, "--sessions", sessions, "--out", out,
		"--gateway", gateway, "--gateway-key-file", keyFile(t), "--draws", "50", "--exposure-log", filepath.Join(t.TempDir(), "exposure.log"))
	if code != 0 {
		t.Fatalf("score = %d: %s", code, stderr)
	}
	results, err := readResults(filepath.Join(out, "cases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for id, r := range results {
		p := r.Grades[JPhrases]
		switch {
		case p == nil || p.Error != "":
			t.Errorf("%s: the phrases judgement: %+v", id, p)
		case id == "phrase/comment:f4@40" && (len(p.Refused) != 1 || p.Refused[0].Model != grader || p.Call == nil || p.Call.Model != first):
			t.Errorf("%s: the phrases judgement the grader refused: %+v", id, p)
		}
		u := r.Grades[JUndefined]
		if u == nil || u.Error != "" || len(u.Refused) != 2 || u.Refused[1].Model != first || u.Call == nil || u.Call.Model != second {
			t.Errorf("%s: the undefined judgement the grader and the first fallback refused: %+v", id, u)
		}
		if s := r.Grades[JAnswerStart]; s != nil && (len(s.Refused) != 0 || s.Call.Model != grader) {
			t.Errorf("%s: the answer_start judgement the grader gave: %+v", id, s)
		}
	}
	if f := results["phrase/comment:f4@40"]; f.Grades[JPhrases].Count != 1 {
		t.Errorf("the fallback's grade of the phrase case: %+v", f.Grades[JPhrases])
	}
}

func TestARunGradesAnotherRunsReadersAgain(t *testing.T) {
	build, sessions := writeFixture(t)
	args := func(stand *modeltest.Model, out string, extra ...string) []string {
		return append([]string{"score", "--cases", build, "--sessions", sessions, "--out", out,
			"--gateway", stand.URL, "--gateway-key-file", keyFile(t), "--draws", "50", "--exposure-log", filepath.Join(t.TempDir(), "exposure.log")}, extra...)
	}
	first := modeltest.New(t, graderRules(t)...)
	outA := t.TempDir()
	if code, _, stderr := runHarness(args(first, outA)...); code != 0 {
		t.Fatalf("the first run = %d: %s", code, stderr)
	}
	second := modeltest.New(t, graderRules(t)...)
	outB := t.TempDir()
	if code, _, stderr := runHarness(args(second, outB, "--readers-from", outA)...); code != 0 {
		t.Fatalf("the second run = %d: %s", code, stderr)
	}
	if n := len(requestsOf(second, markReader)); n != 0 {
		t.Errorf("the second run called the reader %d times", n)
	}
	a, _ := readResults(filepath.Join(outA, "cases.jsonl"))
	b, _ := readResults(filepath.Join(outB, "cases.jsonl"))
	q := b["question/comment:q1@11"]
	if q.Reader == nil || !q.Reader.Reused || q.Reader.Text != a["question/comment:q1@11"].Reader.Text {
		t.Errorf("the second run's reader: %+v", q.Reader)
	}
	grading := 0
	for _, g := range q.Grades {
		if g.Call != nil {
			grading++
		}
	}
	if _, measure := q.calls(); len(measure) != grading {
		t.Errorf("the second run counts %d calls of the question case, want its %d grading calls", len(measure), grading)
	}

	// The two runs of one grader agree on every judgement, and a score that reads the
	// comparison gives it beside each score.
	rep := filepath.Join(t.TempDir(), "repeatability.json")
	code, stdout, stderr := runHarness("compare", "--a", outA, "--b", outB, "--out", rep, "--draws", "50")
	if code != 0 {
		t.Fatalf("compare = %d: %s", code, stderr)
	}
	r, err := readRepeatability(rep)
	if err != nil {
		t.Fatal(err)
	}
	if r.Cases != 4 || r.of(JPhrases) == nil || *r.of(JPhrases).Agreement.Share != 1 || r.of(JLost) != nil {
		t.Errorf("the repeatability: %+v\n%s", r, stdout)
	}
	if code, _, stderr := runHarness(append(args(second, outB, "--readers-from", outA), "--figures-only", "--repeatability", rep)...); code != 0 {
		t.Fatalf("score --figures-only --repeatability = %d: %s", code, stderr)
	}
	md, err := os.ReadFile(filepath.Join(outB, "scores.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "| repeatability |") || !strings.Contains(string(md), "| phrases that state no fact: per text |") ||
		!strings.Contains(string(md), "| 100.0% |") {
		t.Errorf("scores.md gives no repeatability:\n%s", md)
	}
}

func TestCompareRuns(t *testing.T) {
	yes, no := true, false
	grade := func(count int, verdict *bool) *Grade { return &Grade{Count: count, Verdict: verdict} }
	a := map[string]CaseResult{}
	b := map[string]CaseResult{}
	for i, c := range []struct {
		countA, countB     int
		verdictA, verdictB *bool
	}{
		{0, 0, &yes, &yes}, {3, 4, &yes, &no}, {1, 3, &no, &no}, {2, 0, &yes, &yes},
	} {
		id := string(rune('a' + i))
		a[id] = CaseResult{CaseID: id, SplitGroup: id, Grades: map[Judgement]*Grade{JPhrases: grade(c.countA, nil), JHarder: grade(0, c.verdictA)}}
		b[id] = CaseResult{CaseID: id, SplitGroup: id, Grades: map[Judgement]*Grade{JPhrases: grade(c.countB, nil), JHarder: grade(0, c.verdictB)}}
	}
	b["e"] = CaseResult{CaseID: "e", SplitGroup: "e", Error: "a case only one run holds is left out"}
	n, agreements := compareRuns(a, b, 1, 200)
	if n != 4 {
		t.Fatalf("cases = %d, want 4", n)
	}
	got := map[Judgement]JudgementAgreement{}
	for _, ja := range agreements {
		got[ja.Judgement] = ja
	}
	// Phrases: counts within one in cases a and b; any finding in both or neither in a, b and c.
	if p := got[JPhrases]; *p.Agreement.Share != 0.5 || *p.AnyAgreement.Share != 0.75 || *p.MeanCountDifference != 1.25 {
		t.Errorf("phrases: %+v (any %+v, difference %v)", p.Agreement, p.AnyAgreement, *p.MeanCountDifference)
	}
	if h := got[JHarder]; *h.Agreement.Share != 0.75 || h.AnyAgreement != nil {
		t.Errorf("harder: %+v", h)
	}
}

func TestARunGivesOnlyTheJudgementsItNames(t *testing.T) {
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, graderRules(t)...)
	out := t.TempDir()
	code, _, stderr := runHarness("score", "--cases", build, "--sessions", sessions, "--out", out, "--judgements", "undefined",
		"--gateway", stand.URL, "--gateway-key-file", keyFile(t), "--draws", "50", "--exposure-log", filepath.Join(t.TempDir(), "exposure.log"))
	if code != 0 {
		t.Fatalf("score --judgements undefined = %d: %s", code, stderr)
	}
	results, err := readResults(filepath.Join(out, "cases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for id, r := range results {
		if len(r.Grades) != 1 || r.Grades[JUndefined] == nil || r.Reader != nil {
			t.Errorf("%s: grades %v, reader %+v", id, r.Grades, r.Reader)
		}
	}
	if n := len(requestsOf(stand, markReader)); n != 0 {
		t.Errorf("the run called the reader %d times", n)
	}
	if code, _, stderr := runHarness("score", "--cases", build, "--sessions", sessions, "--out", t.TempDir(), "--judgements", "nothing",
		"--gateway", stand.URL, "--gateway-key-file", keyFile(t)); code != 2 {
		t.Errorf("an unknown judgement = %d: %s", code, stderr)
	}
}
