package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
)

func TestWriteLabel(t *testing.T) {
	s := func(verdicts ...string) []sentenceLabel {
		var out []sentenceLabel
		for _, v := range verdicts {
			out = append(out, sentenceLabel{Verdict: v})
		}
		return out
	}
	for _, c := range []struct {
		in   []sentenceLabel
		want string
	}{
		{nil, writePass},
		{s(verdictNotVacuous, verdictNotVacuous), writePass},
		{s(verdictNotVacuous, verdictUnresolved), writeUnresolved},
		{s(verdictUnresolved, verdictVacuous), writeFailure},
	} {
		if got := writeLabelOf(c.in); got != c.want {
			t.Errorf("writeLabelOf(%v) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestRetryableDetail(t *testing.T) {
	for detail, want := range map[string]bool{
		"aisdk: API call error (status 429): slow down":   true,
		"aisdk: API call error (status 529): overloaded":  true,
		"aisdk: API call error (status 400): bad request": false,
		"read tcp: connection reset by peer":              true,
	} {
		if got := retryableDetail(detail); got != want {
			t.Errorf("retryableDetail(%q) = %v, want %v", detail, got, want)
		}
	}
}

// fakeReplies is a stand-in for agentloop.Run: it fails the first failures calls with a 529,
// answers the next partial calls with the first sentence alone, and then labels every sentence
// sent, vacuous where it says "frames".
type fakeReplies struct {
	failures, partial int
	calls             []labelInput
	asks              []string
	sleeps            []time.Duration
}

func (f *fakeReplies) run(_ context.Context, req agentloop.Request) (agentloop.Result, error) {
	if f.failures > 0 {
		f.failures--
		return agentloop.Result{Fault: agentloop.FaultProviderError, Detail: "aisdk: API call error (status 529): overloaded"}, nil
	}
	var in labelInput
	if err := json.Unmarshal([]byte(req.Input[0].Blocks[0].Text), &in); err != nil {
		return agentloop.Result{}, err
	}
	f.calls = append(f.calls, in)
	f.asks = append(f.asks, req.Input[0].Blocks[1].Text)
	var reply struct {
		Sentences []sentenceLabel `json:"sentences"`
	}
	for _, s := range in.Sentences {
		verdict := verdictNotVacuous
		if strings.Contains(s.Text, "frames") {
			verdict = verdictVacuous
		}
		reply.Sentences = append(reply.Sentences, sentenceLabel{ID: s.ID, Verdict: verdict, Shape: shapeOther})
	}
	if f.partial > 0 {
		f.partial--
		reply.Sentences = reply.Sentences[:1]
	}
	value, _ := json.Marshal(reply)
	return agentloop.Result{Value: value, Steps: []agentloop.Step{{Model: "served-model"}},
		Usage: agentloop.Usage{Input: 100, Output: 10}}, nil
}

func TestLabellerRetriesChunksAndLabelsTheWrite(t *testing.T) {
	f := &fakeReplies{failures: 2}
	l := &labeller{system: "rubric", chunk: 2, call: f.run, sleep: func(_ context.Context, d time.Duration) bool {
		f.sleeps = append(f.sleeps, d)
		return true
	}}
	job := labelJob{
		key: writeKey{EventID: 1, SourceID: "c1"}, family: "comment", title: "Fix the lamp",
		cases: []Case{{Field: "body", Surface: surfaceComment,
			Text: "The lamp is on the desk. The cup is blue.\n\nThe plan frames the risk. The kettle boils.\n\nThe cat sleeps."}},
	}
	rec := l.label(context.Background(), job)
	if rec.Error != "" || rec.WriteLabel != writeFailure {
		t.Fatalf("label = %s, error %q", rec.WriteLabel, rec.Error)
	}
	if len(rec.Sentences) != 5 || rec.Sentences[2].Verdict != verdictVacuous || rec.Sentences[0].Verdict != verdictNotVacuous {
		t.Errorf("sentences = %+v", rec.Sentences)
	}
	if len(rec.Calls) != 3 || rec.Calls[0].Attempts != 3 || rec.Calls[0].Model != "served-model" || rec.Calls[1].Attempts != 1 {
		t.Errorf("calls = %+v", rec.Calls)
	}
	if len(f.sleeps) != 2 || f.sleeps[0] != labelBackoff || f.sleeps[1] != 2*labelBackoff {
		t.Errorf("backoff = %v", f.sleeps)
	}
	// The middle chunk carries its neighbours, the raw text and the issue title.
	mid := f.calls[1]
	if len(mid.Sentences) != 2 || mid.Sentences[0].ID != "s3" || len(mid.Context) != 3 ||
		mid.Fields[0].Text != job.cases[0].Text || mid.IssueTitle != "Fix the lamp" {
		t.Errorf("middle chunk = %+v", mid)
	}

	f = &fakeReplies{failures: labelRetries + 1}
	l.call = f.run
	if rec := l.label(context.Background(), job); rec.WriteLabel != writeError || !strings.Contains(rec.Error, "529") {
		t.Errorf("after %d failures: label %s, error %q", labelRetries+1, rec.WriteLabel, rec.Error)
	}

	f = &fakeReplies{partial: labelReplyRetries}
	l.call = f.run
	rec = l.label(context.Background(), job)
	if rec.WriteLabel != writeFailure || rec.Calls[0].Attempts != labelReplyRetries+1 || rec.Calls[0].InputTokens != 100*(labelReplyRetries+1) {
		t.Errorf("after %d partial replies: label %s, calls %+v", labelReplyRetries, rec.WriteLabel, rec.Calls)
	}
	if f.asks[0] != "Label all 2 numbered sentences, s1 to s2, with one entry each." {
		t.Errorf("the call asks %q", f.asks[0])
	}
	f = &fakeReplies{partial: labelReplyRetries + 1}
	l.call = f.run
	if rec := l.label(context.Background(), job); rec.WriteLabel != writeError || rec.Calls[0].Reply == nil {
		t.Errorf("after %d partial replies: label %s, calls %+v", labelReplyRetries+1, rec.WriteLabel, rec.Calls)
	}
}

func TestVerdictsByIDRefusesAMissingOrUnknownSentence(t *testing.T) {
	sent := []labelText{{ID: "s1"}, {ID: "s2"}}
	for _, reply := range []string{
		`{"sentences":[{"id":"s1","verdict":"vacuous","shape":"names","rewrite":"x","reason":"y"}]}`,
		`{"sentences":[{"id":"s1","verdict":"vacuous","shape":"names","rewrite":"x","reason":"y"},{"id":"s3","verdict":"vacuous","shape":"names","rewrite":"x","reason":"y"}]}`,
		`{"sentences":[{"id":"s1","verdict":"maybe","shape":"names","rewrite":"x","reason":"y"},{"id":"s2","verdict":"vacuous","shape":"names","rewrite":"x","reason":"y"}]}`,
	} {
		if _, err := verdictsByID(json.RawMessage(reply), sent); err == nil {
			t.Errorf("reply %s was taken", reply)
		}
	}
}

func TestChunksCutALongWriteWithoutItsRawText(t *testing.T) {
	ranges, raw := chunks(130, labelContextChars+1, 60)
	if raw || len(ranges) != 3 || ranges[2] != [2]int{120, 130} {
		t.Errorf("chunks = %v, raw %v", ranges, raw)
	}
	if ranges, raw := chunks(10, 500, 60); !raw || len(ranges) != 1 {
		t.Errorf("a short write: chunks = %v, raw %v", ranges, raw)
	}
}
