package main

// The commands of package main that reuse, refuse or fail a run: a run that reuses earlier results
// only under its own inputs, a run whose calls failed or stopped that fails, an output a
// repository or an input does not take, and a claim made after every refusal that reads no write
// (dispatch://INFRA-3231).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/cmd/writing-check/harness"
	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/eval"
)

// labelFixture is a cases file of one comment, a definition and a key file in dir.
func labelFixture(t *testing.T, dir, text string) (cases, definition, key string) {
	t.Helper()
	cases, definition, key = filepath.Join(dir, "cases.jsonl"), filepath.Join(dir, "definition.md"), filepath.Join(dir, "key")
	writeJSONLines(t, cases, Case{EventID: 7, SourceID: "c7", Surface: "comment", Field: "body", Text: text})
	for path, data := range map[string]string{definition: "A verb phrase is vacuous when it states no fact.\n", key: "stub-key\n"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cases, definition, key
}

// labelArgs are label's flags against the stub.
func labelArgs(stub *stubModel, cases, definition, key, out string, extra ...string) []string {
	return append([]string{"label", "--in", cases, "--definition", definition, "--out", out, "--kind", "openai-chat",
		"--endpoint", stub.server.URL + "/", "--key-file", key, "--concurrency", "1"}, extra...)
}

// The top-level score writes outputs.jsonl, which holds sentences of the labelled texts: an --out
// inside a repository is refused before any input is read, and nothing is made there.
func TestScoreRefusesAnOutInsideARepository(t *testing.T) {
	// The refusal names the repository with its links resolved (t.TempDir is under a link on
	// macOS).
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	stub := newStubModel(t, containsAny("names the"), nil)
	inside := filepath.Join(repo, "data", "out")
	code, _, stderr := runCmd(t, append([]string{"score", "--labels", filepath.Join(t.TempDir(), "labels.jsonl"), "--stratum", stratumReal, "--out", inside}, stub.args(t, t.TempDir())...)...)
	if code != 2 || !strings.Contains(stderr, " is inside the repository at "+repo+": its files hold Dispatch text") {
		t.Errorf("score --out inside a repository = %d:\n%s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(repo, "data")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused score made %s (%v)", filepath.Join(repo, "data"), err)
	}
}

// A final-test score whose --out cannot be made fails before it claims the set: the exposure log
// holds no run, so the set is not spent on a run that read nothing.
func TestAFinalTestWhoseOutCannotBeMadeSpendsNoSet(t *testing.T) {
	dir := t.TempDir()
	labels, manifest := fixtureLabels(t, dir)
	blocker := filepath.Join(dir, "a-file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "exposure.jsonl")
	stub := newStubModel(t, containsAny("names the"), nil)
	args := append([]string{"score", "--labels", labels, "--planted", manifest, "--stratum", stratumReal, "--out", filepath.Join(blocker, "out"),
		"--root", "../..", "--final-test", "--set-digest", strings.Repeat("ab", 32), "--seed", "42", "--exposure-log", log}, stub.args(t, dir)...)
	code, _, stderr := runCmd(t, args...)
	if code != 1 || !strings.Contains(stderr, "not a directory") {
		t.Errorf("a final test whose --out cannot be made = %d:\n%s", code, stderr)
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the run that read no write wrote the exposure log (%v)", err)
	}
	if stub.calls.Load() != 0 {
		t.Error("the refused run called the model")
	}
}

// A final-test score makes the --out it writes its files in: --out work/link/../o is work/o, the
// name filepath.Join gives each file, and not x/o, where the kernel takes the name. So the run
// writes its files in work/o and makes no x/o; and an --out work/link/../f, where the file work/f
// stands, cannot be made, though the kernel would make x/f, so that run fails before it claims
// the set: the exposure log holds no run, and no reader is called.
func TestAFinalTestMakesTheOutItWrites(t *testing.T) {
	work, x := linkedDirs(t)
	dir := t.TempDir()
	labels, manifest := fixtureLabels(t, dir)
	stub := newStubModel(t, containsAny("names the"), nil)
	final := func(out, log string) (int, string) {
		t.Helper()
		args := append([]string{"score", "--labels", labels, "--planted", manifest, "--stratum", stratumReal, "--out", out,
			"--root", "../..", "--final-test", "--set-digest", strings.Repeat("ab", 32), "--seed", "42", "--exposure-log", log}, stub.args(t, dir)...)
		code, _, stderr := runCmd(t, args...)
		return code, stderr
	}
	if err := os.WriteFile(filepath.Join(work, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	refused := filepath.Join(dir, "refused.jsonl")
	// Written out, as linkedDirs says.
	if code, stderr := final(work+"/link/../f", refused); code != 1 || !strings.Contains(stderr, "not a directory") {
		t.Errorf("a final test whose --out work/link/../f cannot be made = %d, want 1 and the refusal:\n%s", code, stderr)
	}
	if _, err := os.Stat(refused); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the final test whose --out cannot be made wrote the exposure log (%v)", err)
	}
	if n := stub.calls.Load(); n != 0 {
		t.Errorf("the final test whose --out cannot be made made %d reader calls", n)
	}
	if code, stderr := final(work+"/link/../o", filepath.Join(dir, "exposure.jsonl")); code != 0 {
		t.Errorf("a final test with --out work/link/../o = %d:\n%s", code, stderr)
	}
	if names, err := dirNames(filepath.Join(work, "o")); err != nil || !slices.Equal(names, []string{"figures.json", "figures.md", "outputs.jsonl"}) {
		t.Errorf("work/o holds %v (%v), want the run's three files", names, err)
	}
	for _, name := range []string{"f", "o"} {
		if _, err := os.Stat(filepath.Join(x, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the final tests made x/%s (%v), a directory they write nothing in", name, err)
		}
	}
}

// A reading makes the --out it writes its files in, as a final-test score does
// (TestAFinalTestMakesTheOutItWrites): --out work/link/../o is work/o, and it makes no x/o; and an
// --out work/link/../f, where the file work/f stands, cannot be made, so that reading fails before
// its first detector call.
func TestAReadingMakesTheOutItWrites(t *testing.T) {
	work, x := linkedDirs(t)
	dir := t.TempDir()
	in := writeCases(t, dir, "cases.jsonl", []Case{
		{EventID: 210, SourceID: "r1", Surface: surfaceComment, Field: "body", Text: sentA, Project: "ALPHA"},
		{EventID: 220, SourceID: "r2", Surface: surfaceComment, Field: "body", Text: sentH, Project: "BETA"},
	})
	stub := newStubModel(t, containsAny("names the"), nil)
	reading := func(out string) (int, string) {
		t.Helper()
		code, _, stderr := runCmd(t, append([]string{"reading", "--in", in, "--after", "200", "--through", "300", "--out", out, "--root", "../.."}, stub.args(t, dir)...)...)
		return code, stderr
	}
	if err := os.WriteFile(filepath.Join(work, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Written out, as linkedDirs says.
	if code, stderr := reading(work + "/link/../f"); code != 1 || !strings.Contains(stderr, "not a directory") {
		t.Errorf("a reading whose --out work/link/../f cannot be made = %d, want 1 and the refusal:\n%s", code, stderr)
	}
	if n := stub.calls.Load(); n != 0 {
		t.Errorf("the reading whose --out cannot be made made %d detector calls", n)
	}
	if code, stderr := reading(work + "/link/../o"); code != 0 {
		t.Errorf("a reading with --out work/link/../o = %d:\n%s", code, stderr)
	}
	want := []string{"manifest.jsonl", "manifest.sha256", "report.json", "report.md", "results.jsonl"}
	if names, err := dirNames(filepath.Join(work, "o")); err != nil || !slices.Equal(names, want) {
		t.Errorf("work/o holds %v (%v), want %v", names, err, want)
	}
	for _, name := range []string{"f", "o"} {
		if _, err := os.Stat(filepath.Join(x, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the readings made x/%s (%v), a directory they write nothing in", name, err)
		}
	}
}

// A final-test score whose reader key file cannot be read fails before it claims the set: every
// call would fail with no write read, so the exposure log holds no run, and no --out is made.
func TestAFinalTestWhoseKeyCannotBeReadSpendsNoSet(t *testing.T) {
	dir := t.TempDir()
	labels, manifest := fixtureLabels(t, dir)
	log := filepath.Join(dir, "exposure.jsonl")
	out := filepath.Join(dir, "out")
	stub := newStubModel(t, containsAny("names the"), nil)
	args := append([]string{"score", "--labels", labels, "--planted", manifest, "--stratum", stratumReal, "--out", out,
		"--root", "../..", "--final-test", "--set-digest", strings.Repeat("ab", 32), "--seed", "42", "--exposure-log", log}, stub.args(t, dir)...)
	code, _, stderr := runCmd(t, append(args, "--key-file", filepath.Join(dir, "no-key"))...)
	if code != 1 || !strings.Contains(stderr, "--key-file: open "+filepath.Join(dir, "no-key")) {
		t.Errorf("a final test whose key file cannot be read = %d:\n%s", code, stderr)
	}
	for _, made := range []string{log, out} {
		if _, err := os.Stat(made); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the refused final test made %s (%v)", made, err)
		}
	}
	if stub.calls.Load() != 0 {
		t.Error("the refused run called the model")
	}
}

// A final-test score whose reader key file or federated token file holds nothing fails before it
// claims the set, as one it cannot read does: the Entra sign-in refuses an empty token file on the
// first call alone, and no call signs in with an empty key, so every call would fail with no write
// read. The exposure log holds no run, and no --out is made.
func TestAFinalTestWhoseCredentialFileIsEmptySpendsNoSet(t *testing.T) {
	dir := t.TempDir()
	labels, manifest := fixtureLabels(t, dir)
	stub := newStubModel(t, containsAny("names the"), nil)
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := []string{"--kind", "openai-chat", "--endpoint", stub.server.URL + "/", "--deployment", "stub-deployment", "--deadline", "10s",
		"--model-id", stubModelID, "--concurrency", "2"}
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"key file", []string{"--key-file", empty}, "--key-file " + empty + " holds no credential"},
		{"federated token file", []string{"--azure-tenant", "tenant", "--azure-client-id", "client", "--federated-token-file", empty},
			"--federated-token-file " + empty + " holds no credential"},
	} {
		log, out := filepath.Join(dir, c.name+".jsonl"), filepath.Join(dir, c.name)
		args := slices.Concat([]string{"score", "--labels", labels, "--planted", manifest, "--stratum", stratumReal, "--out", out, "--root", "../..",
			"--final-test", "--set-digest", strings.Repeat("ab", 32), "--seed", "42", "--exposure-log", log}, reader, c.args)
		if code, _, stderr := runCmd(t, args...); code != 1 || !strings.Contains(stderr, c.want) {
			t.Errorf("a final test whose %s is empty = %d:\n%s\nwant 1 and %q", c.name, code, stderr, c.want)
		}
		for _, made := range []string{log, out} {
			if _, err := os.Stat(made); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the final test refused on its empty %s made %s (%v)", c.name, made, err)
			}
		}
	}
	if stub.calls.Load() != 0 {
		t.Error("a refused run called the model")
	}
}

// Only a final test reads the reader's key before it scores, so that its claim follows the read.
// Any other run reads the key on each reader call alone: one over writes that hold no candidate
// calls no reader, and scores with a key file it cannot read. A final test of the same writes is
// refused on that file before its claim, and the exposure log holds no run.
func TestOnlyAFinalTestReadsTheReadersKeyBeforeItScores(t *testing.T) {
	dir := t.TempDir()
	labels := filepath.Join(dir, "labels.jsonl")
	fact := func(event int64, source, text string) labelRecord {
		r := labelRecord{EventID: event, SourceID: source, Family: "comment",
			Sentences: []sentenceLabel{{ID: "s1", Field: "body", Kind: "paragraph", Text: text, Verdict: verdictNotVacuous, Shape: "none"}}}
		r.WriteLabel = writeLabelOf(r.Sentences)
		return r
	}
	if _, err := writeJSONL(labels, []labelRecord{fact(101, "w1", sentB), fact(102, "w2", sentM)}); err != nil {
		t.Fatal(err)
	}
	stub := newStubModel(t, containsAny("names the"), nil)
	noKey := filepath.Join(dir, "no-key")
	args := append(append([]string{"score", "--labels", labels, "--stratum", stratumReal, "--root", "../.."}, stub.args(t, dir)...), "--key-file", noKey)
	out := filepath.Join(dir, "out")
	if code, _, stderr := runCmd(t, append(args, "--out", out)...); code != 0 || stub.calls.Load() != 0 {
		t.Errorf("a score of writes with no candidate, with a key file it cannot read = %d after %d calls, want 0 after none:\n%s", code, stub.calls.Load(), stderr)
	}
	if _, err := os.Stat(filepath.Join(out, "figures.json")); err != nil {
		t.Errorf("the score of writes with no candidate wrote no figures (%v)", err)
	}
	log := filepath.Join(dir, "exposure.jsonl")
	code, _, stderr := runCmd(t, append(args, "--out", filepath.Join(dir, "final"), "--final-test", "--set-digest", strings.Repeat("ab", 32), "--seed", "42", "--exposure-log", log)...)
	if code != 1 || !strings.Contains(stderr, "--key-file: open "+noKey) {
		t.Errorf("a final test of the same writes, with a key file it cannot read = %d, want 1 and the key file's refusal:\n%s", code, stderr)
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the final test refused on its key file wrote the exposure log (%v)", err)
	}
}

// label --resume keeps a label only where the run's definition, model, effort and chunk, and the
// write's text, are those it was made with: a resume after the definition or the text changed is
// refused before any call, and a resume under the same ones keeps the label and calls nothing.
func TestLabelResumeRefusesLabelsOfOtherInputs(t *testing.T) {
	dir := t.TempDir()
	text := "The fix names the risk."
	cases, definition, key := labelFixture(t, dir, text)
	rubric, err := os.ReadFile(definition)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := labelJobs([]Case{{EventID: 7, SourceID: "c7", Surface: "comment", Field: "body", Text: text}}, func(string, int64) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	record := func(params, input string) labelRecord {
		return labelRecord{EventID: 7, SourceID: "c7", Family: "comment", WriteLabel: writePass, Params: params, Input: input,
			Sentences: []sentenceLabel{{ID: "s1", Field: "body", Text: text, Verdict: verdictNotVacuous, Shape: "none"}}, Calls: []labelCall{}}
	}
	same := labelParams(labelPreamble+string(rubric), "claude-opus-5-5", "off", 60)
	other := labelParams(labelPreamble+"An earlier definition.\n", "claude-opus-5-5", "off", 60)
	stub := newStubModel(t, containsAny("names the"), nil)
	for _, c := range []struct {
		name   string
		record labelRecord
		code   int
		want   string
	}{
		{"another definition", record(other, jobs[0].input), 2, "made with another definition, model, effort or --chunk"},
		{"another text", record(same, sha256Hex([]byte("another text"))), 2, "whose text or issue title in --in is not the one they were labelled from"},
		{"the same inputs", record(same, jobs[0].input), 0, "1 writes, 1 already labelled, 0 to label"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "labels.jsonl")
			writeJSONLines(t, out, c.record)
			before := stub.calls.Load()
			code, _, stderr := runCmd(t, labelArgs(stub, cases, definition, key, out, "--resume")...)
			if code != c.code || !strings.Contains(stderr, c.want) {
				t.Errorf("label --resume = %d, want %d and %q:\n%s", code, c.code, c.want, stderr)
			}
			if stub.calls.Load() != before {
				t.Errorf("the resume made %d calls", stub.calls.Load()-before)
			}
		})
	}
}

// label refuses an --out that names its --in or its --titles file, which the labels would be
// renamed over.
func TestLabelRefusesAnOutThatIsItsInput(t *testing.T) {
	dir := t.TempDir()
	cases, definition, key := labelFixture(t, dir, "The fix names the risk.")
	before, err := os.ReadFile(cases)
	if err != nil {
		t.Fatal(err)
	}
	stub := newStubModel(t, containsAny("names the"), nil)
	titles := filepath.Join(dir, "titles.jsonl")
	if err := os.WriteFile(titles, before, 0o600); err != nil {
		t.Fatal(err)
	}
	for flag, out := range map[string]string{"--in": cases, "--titles": titles} {
		code, _, stderr := runCmd(t, labelArgs(stub, cases, definition, key, out, "--resume", "--titles", titles)...)
		if code != 2 || !strings.Contains(stderr, "--out names the "+flag+" file") {
			t.Errorf("label --out %s = %d:\n%s", flag, code, stderr)
		}
	}
	if after, err := os.ReadFile(cases); err != nil || string(after) != string(before) {
		t.Errorf("the refused label changed its --in (%v)", err)
	}
	if stub.calls.Load() != 0 {
		t.Error("the refused label called the model")
	}
}

// A label run in which a write's calls failed leaves that write labelled "error", counts the
// failed calls in its summary and exits 1, so the run does not read as complete.
func TestALabelRunWithFailedCallsExitsOne(t *testing.T) {
	dir := t.TempDir()
	cases, definition, key := labelFixture(t, dir, "The fix names the risk.")
	// The stub answers the reader's schema, which is not the labeller's: every reply is refused.
	stub := newStubModel(t, containsAny("names the"), nil)
	out := filepath.Join(dir, "labels.jsonl")
	code, stdout, stderr := runCmd(t, labelArgs(stub, cases, definition, key, out)...)
	if code != 1 || !strings.Contains(stderr, "1 of 1 writes have no label, as their calls failed") || !strings.Contains(stdout, "failed 1)") {
		t.Errorf("a label run whose calls failed = %d:\n%s%s", code, stdout, stderr)
	}
}

// probeArgs are probe-model's flags against the stub.
func probeArgs(t *testing.T, stub *stubModel, extra ...string) []string {
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("stub-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return append([]string{"probe-model", "--kind", "openai-chat", "--endpoint", stub.server.URL + "/", "--deployment", "stub-deployment",
		"--key-file", key, "--deadline", "10s"}, extra...)
}

// probe-model exits 0 when every call read its candidates, and 1 when a call failed, when it
// stopped with fewer calls than asked, or when --out could not take a record.
func TestProbeModelFailsOnAFailedCallAStopOrAWriteError(t *testing.T) {
	out := filepath.Join(t.TempDir(), "calls.jsonl")
	if code, stdout, stderr := runCmd(t, probeArgs(t, newStubModel(t, containsAny("names the"), nil), "--calls", "3", "--out", out)...); code != 0 {
		t.Errorf("a probe whose calls all answered = %d:\n%s%s", code, stdout, stderr)
	}
	if lines := readJSONL[probeCall](t, out); len(lines) != 3 {
		t.Errorf("--out holds %d records, want 3", len(lines))
	}
	failing := newStubModel(t, containsAny("names the"), func(string) bool { return true })
	if code, _, stderr := runCmd(t, probeArgs(t, failing, "--calls", "2")...); code != 1 || !strings.Contains(stderr, "2 of 2 calls failed") {
		t.Errorf("a probe whose calls failed = %d:\n%s", code, stderr)
	}
	if _, err := os.Stat("/dev/full"); err == nil {
		code, _, stderr := runCmd(t, probeArgs(t, newStubModel(t, containsAny("names the"), nil), "--calls", "2", "--out", "/dev/full")...)
		if code != 1 || !strings.Contains(stderr, "writing /dev/full") {
			t.Errorf("a probe whose --out takes no record = %d:\n%s", code, stderr)
		}
	}
	// The first call's reply cancels the run's context: the probe stops after one call of three.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stopping := newStubModel(t, func(string) bool { cancel(); return false }, nil)
	var stdout, stderr strings.Builder
	if code := run(ctx, probeArgs(t, stopping, "--calls", "3"), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "stopped (context canceled) after 1 of 3 calls") {
		t.Errorf("a probe stopped after its first call = %d:\n%s", code, stderr.String())
	}
}

// validate refuses an --exposure-log that does not exist once the description names the
// final-test set, and prints the log it read: a mistyped path would otherwise read as a log in
// which no run read the set.
func TestValidateRefusesAMissingExposureLog(t *testing.T) {
	f := newValidFixture(t)
	f.d.FinalTest = &eval.FinalTest{Pending: eval.Pending{Pending: true, Reason: "later"}}
	f.runs = nil
	f.write(t)
	code, stdout, _ := runCmd(t, "validate", "--description", f.desc, "--exposure-log", f.log)
	if code != 0 || !strings.Contains(stdout, "exposure log: "+f.log+", 0 runs") {
		t.Fatalf("a description before its claim with an empty log = %d:\n%s", code, stdout)
	}
	if err := os.Remove(f.log); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ = runCmd(t, "validate", "--description", f.desc, "--exposure-log", f.log)
	if code != 1 || !strings.Contains(stdout, "the --exposure-log file does not exist, and the description names the final-test set") ||
		!strings.Contains(stdout, f.log+" does not exist") {
		t.Errorf("a missing exposure log = %d:\n%s", code, stdout)
	}
}

// A regress case whose reader call failed is a fault, not a disagreement: the classes count the
// cases read, and a run with a fault exits 1 under any bound.
func TestRegressCountsAFailedCallApartAndFails(t *testing.T) {
	dir := t.TempDir()
	cases := []regressCase{
		{ID: "r1", Body: sentA + " " + sentB, Expect: []regressVerdict{{sentA, true}, {sentB, false}}},
		{ID: "r2", Body: sentH, Expect: []regressVerdict{{sentH, false}}},
		{ID: "r3", Body: sentC + "\n\n" + sentM, Expect: []regressVerdict{{sentC, true}, {sentM, false}}},
	}
	path := filepath.Join(dir, "cases.jsonl")
	if _, err := writeJSONL(path, cases); err != nil {
		t.Fatal(err)
	}
	failing := newStubModel(t, containsAny("names the"), containsAny("shows the"))
	out := filepath.Join(dir, "regress.json")
	code, stdout, stderr := runCmd(t, append([]string{"regress", "--cases", path, "--repeat-bound", "1", "--out", out}, failing.args(t, dir)...)...)
	if code != 1 || !strings.Contains(stderr, "1 of 3 cases were not read whole") || !strings.Contains(stdout, "- r3: reader_error, 2 expected sentences unread") {
		t.Errorf("regress with a failed call at a bound of 100%% = %d:\n%s%s", code, stdout, stderr)
	}
	var report regressReport
	data, _ := os.ReadFile(out)
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if c := report.Classes; len(report.Faults) != 1 || len(report.Misses) != 0 || c[0].N != 1 || c[1].N != 2 || report.Pass {
		t.Errorf("the report: classes %+v, faults %+v, misses %+v, pass %t", c, report.Faults, report.Misses, report.Pass)
	}
}

// readers --state continues a replayed state only from the Dispatch it replayed: another
// DISPATCH_URL is refused before any read.
func TestReadersRefusesAStateOfAnotherDispatch(t *testing.T) {
	dir := t.TempDir()
	in, state := filepath.Join(dir, "cases.jsonl"), filepath.Join(dir, "state.json")
	writeJSONLines(t, in, Case{EventID: 3, SourceID: "c3", Surface: "comment", Field: "body", Text: "The run is green."})
	s := newReplayState(0)
	s.DispatchURL, s.Through, s.Events = "http://127.0.0.1:9", 2, 2
	if err := saveReplayState(state, s); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISPATCH_URL", "http://127.0.0.1:1")
	t.Setenv("DISPATCH_TOKEN_FILE", filepath.Join(dir, "token"))
	code, _, stderr := runCmd(t, "readers", "--in", in, "--out", filepath.Join(dir, "out.jsonl"), "--log-after", "0", "--log-through", "5", "--state", state)
	if code != 2 || !strings.Contains(stderr, `was replayed from Dispatch "http://127.0.0.1:9", not DISPATCH_URL http://127.0.0.1:1`) {
		t.Errorf("readers --state of another Dispatch = %d:\n%s", code, stderr)
	}
}

// fetch --resume continues cases read from the same Dispatch after the same event only: a partial
// file of another Dispatch, or one whose fetch recorded neither, is refused before any read.
func TestFetchResumeRefusesCasesOfAnotherDispatch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DISPATCH_URL", "http://127.0.0.1:1")
	t.Setenv("DISPATCH_TOKEN_FILE", filepath.Join(dir, "token"))
	out := filepath.Join(dir, "cases.jsonl")
	writeJSONLines(t, out+".partial", Case{EventID: 3, SourceID: "c3", Surface: "comment", Field: "body", Text: "The run is green."})
	args := []string{"fetch", "--after", "0", "--through", "10", "--out", out, "--resume"}
	code, _, stderr := runCmd(t, args...)
	if code != 2 || !strings.Contains(stderr, "which names the Dispatch and the --after they were read from, does not exist") {
		t.Errorf("fetch --resume of a partial file with no record = %d:\n%s", code, stderr)
	}
	writeJSONLines(t, fetchParamsPath(out+".partial"), fetchParams{DispatchURL: "http://127.0.0.1:9", After: 0})
	code, _, stderr = runCmd(t, args...)
	if code != 2 || !strings.Contains(stderr, "was read from http://127.0.0.1:9 after event 0; this run reads http://127.0.0.1:1 after event 0") {
		t.Errorf("fetch --resume of another Dispatch's cases = %d:\n%s", code, stderr)
	}
}

// A dataset fetch's work directory is reused only for the same end, owner and Dispatch.
func TestDatasetFetchWorkIsReusedFromOneDispatchOnly(t *testing.T) {
	dir := t.TempDir()
	if err := dsCheckWorkEnd(dir, fixtureEnd, fixtureOwner, "http://127.0.0.1:9"); err != nil {
		t.Fatal(err)
	}
	if err := dsCheckWorkEnd(dir, fixtureEnd, fixtureOwner, "http://127.0.0.1:9"); err != nil {
		t.Errorf("the same fetch again: %v", err)
	}
	if err := dsCheckWorkEnd(dir, fixtureEnd, fixtureOwner, "http://127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), `from Dispatch "http://127.0.0.1:9"`) {
		t.Errorf("a fetch of another Dispatch into the work directory: %v", err)
	}
}

// classify keeps an earlier verdict only where it was made with this run's prompt and input by a
// model it keeps (--model or a --keep-model); and a text whose last line is an error has no
// verdict, though an earlier line held one.
func TestClassifyKeepsOnlyAVerdictOfItsOwnInputs(t *testing.T) {
	v := dsVerdict{Text: "t1", PromptSHA256: "p", RequestedModel: "m", InputSHA256: "i", Verdict: "about_words"}
	for _, c := range []struct {
		name          string
		prompt, input string
		models        []string
		keep          bool
	}{
		{"the same", "p", "i", []string{"m"}, true},
		{"a kept model", "p", "i", []string{"m2", "m"}, true},
		{"another prompt", "p2", "i", []string{"m"}, false},
		{"another model", "p", "i", []string{"m2"}, false},
		{"another input", "p", "i2", []string{"m"}, false},
	} {
		if got := currentVerdict(v, c.prompt, c.models, c.input); got != c.keep {
			t.Errorf("%s: kept %t, want %t", c.name, got, c.keep)
		}
	}
	path := filepath.Join(t.TempDir(), "verdicts.jsonl")
	writeJSONLines(t, path, v, dsVerdict{Text: "t1", Error: "the call failed"}, dsVerdict{Text: "t2", Verdict: "plain"})
	got, _, err := readVerdicts(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["t1"]; ok || got["t2"].Verdict != "plain" {
		t.Errorf("the verdicts read: %+v", got)
	}
}

// The two passes dataset/DATASET records, through classify and its stand-in model, which refuses
// four texts when the default model is asked: the second pass, --model claude-sonnet-5-5
// --keep-model claude-sonnet-4-6, sends only those four, to the second model, and keeps every
// verdict of the first; a rerun with the default model and --keep-model claude-sonnet-5-5 sends
// nothing and writes no line. Without --keep-model a verdict of another model asked for is
// classified again, and a refusal leaves its text with no verdict, as before.
func TestClassifyKeepsTheVerdictsOfEachModelItKeeps(t *testing.T) {
	const first, second = dsDefaultModel, "claude-sonnet-5-5"
	refused := []string{"I cannot follow this at all.", "What is this list for?", `Do not write "names the"`, `Good, "shows the cost"`}
	// refusedText is the one of refused that the person's part of a request holds, "" for none.
	refusedText := func(r modeltest.Request) string {
		_, rest, _ := strings.Cut(r.Text, "<person_text")
		part, _, _ := strings.Cut(rest, "</person_text>")
		for _, text := range refused {
			if strings.Contains(part, text) {
				return text
			}
		}
		return ""
	}
	stand := modeltest.New(t,
		modeltest.Rule{Model: first, Match: func(r modeltest.Request) bool { return refusedText(r) != "" },
			Reply: modeltest.Reply{Status: http.StatusBadRequest, Text: "the stand-in refuses this text"}},
		modeltest.Rule{Reply: modeltest.Reply{Text: `{"verdict":"none","words":[]}`}},
	)
	dir := t.TempDir()
	snapshot, _ := standardFixture().write(t, dir)
	key, out := filepath.Join(dir, "key"), filepath.Join(dir, "classified.jsonl")
	if err := os.WriteFile(key, []byte("stand-in\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// classify runs one pass on the verdicts file at path, and answers the requests it sent.
	classify := func(path string, flags ...string) (int, string, []modeltest.Request) {
		t.Helper()
		before := len(stand.Requests())
		code, _, stderr := runCmd(t, append([]string{"dataset", "classify", "--snapshot", snapshot, "--out", path, "--endpoint", stand.URL, "--key-file", key}, flags...)...)
		return code, stderr, stand.Requests()[before:]
	}
	verdicts := func(path string) map[string]dsVerdict {
		t.Helper()
		got, _, err := readVerdicts(path)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	code, stderr, calls := classify(out)
	texts := len(calls)
	firstPass := verdicts(out)
	if code != 1 || !strings.Contains(stderr, "4 texts got no verdict") || len(firstPass) != texts-4 {
		t.Fatalf("the first pass = %d, %d verdicts of %d texts, want 1 and all but the 4 refused:\n%s", code, len(firstPass), texts, stderr)
	}

	code, stderr, calls = classify(out, "--model", second, "--keep-model", first)
	var sent []string
	for _, r := range calls {
		sent = append(sent, r.Model+": "+refusedText(r))
	}
	var want []string
	for _, text := range refused {
		want = append(want, second+": "+text)
	}
	slices.Sort(sent)
	slices.Sort(want)
	if code != 0 || !slices.Equal(sent, want) {
		t.Errorf("the second pass = %d and sent %q, want 0 and the 4 refused texts alone, to %s:\n%s", code, sent, second, stderr)
	}
	both, changed := verdicts(out), 0
	for id, v := range firstPass {
		if !reflect.DeepEqual(both[id], v) {
			changed++
		}
	}
	if changed > 0 || len(both) != texts {
		t.Errorf("after the second pass %d of %d texts have a verdict, and %d of the first pass's %d verdicts changed; want all, and none", len(both), texts, changed, len(firstPass))
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	code, stderr, calls = classify(out, "--keep-model", second)
	if after, _ := os.ReadFile(out); code != 0 || len(calls) != 0 || !bytes.Equal(after, data) {
		t.Errorf("a rerun of the default model keeping %s = %d, sent %d texts, and changed the verdicts file %t; want 0, none, unchanged:\n%s", second, code, len(calls), !bytes.Equal(after, data), stderr)
	}

	other := filepath.Join(dir, "other.jsonl")
	if err := os.WriteFile(other, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stderr, calls = classify(other, "--model", second); code != 0 || len(calls) != texts-4 {
		t.Errorf("--model %s with no --keep-model = %d and sent %d texts, want 0 and the %d of %s again:\n%s", second, code, len(calls), texts-4, first, stderr)
	}
	code, stderr, calls = classify(out)
	if got := verdicts(out); code != 1 || len(calls) != 4 || len(got) != texts-4 {
		t.Errorf("the default model with no --keep-model = %d, sent %d texts and left %d verdicts, want 1, the 4 of %s, and %d:\n%s", code, len(calls), len(got), second, texts-4, stderr)
	}
}

// Every command of package main refuses, with exit 2 and before it writes anything, an output
// that is one of its inputs by one path or another: a file it writes, one it writes in its output
// directory, or one in a directory it replaces whole, that is a file another of its flags reads
// (probe-model's --bodies, label's --definition, a key file, the token file and every other
// input), and an output in a directory it reads whole. The input is left as it was.
func TestEveryCommandRefusesAnOutputThatIsAnInput(t *testing.T) {
	dir := t.TempDir()
	// file writes an input at name under dir and answers its path.
	file := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("keep\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	at := func(name string) string { return filepath.Join(dir, name) }
	// The token file is where named writes its counts.json, so one file serves every command that
	// reads it. Dispatch and the model are a port that answers nothing, so a command that went on
	// past its refusal fails at once.
	token := file("named/counts.json")
	t.Setenv("DISPATCH_URL", "http://127.0.0.1:1")
	t.Setenv("DISPATCH_TOKEN_FILE", token)
	key, other := file("key"), file("other.jsonl")
	reader := []string{"--kind", "openai-chat", "--endpoint", "http://127.0.0.1:1/", "--deployment", "stub", "--key-file", key}
	endpoint := []string{"--endpoint", "http://127.0.0.1:1/", "--key-file", key}
	bodies, definition, labelled := file("bodies.jsonl"), file("definition.md"), file("labelled/set/labels.jsonl")
	for _, c := range []struct {
		args         []string
		input, wants string
	}{
		{append([]string{"probe-model", "--bodies", bodies, "--out", bodies}, reader...), bodies, "--out names the --bodies file"},
		{append([]string{"probe-model", "--out", key}, reader...), key, "--out names the --key-file file"},
		{append([]string{"label", "--in", other, "--definition", definition, "--out", definition, "--resume"}, endpoint...), definition, "--out names the --definition file"},
		{append([]string{"label", "--in", other, "--definition", other, "--out", key, "--resume"}, endpoint...), key, "--out names the --key-file file"},
		{[]string{"readers", "--in", file("readers/in.jsonl"), "--state", at("readers/in.jsonl"), "--out", at("readers/out.jsonl"), "--log-through", "10"}, at("readers/in.jsonl"), "--state names the --in file"},
		{[]string{"readers", "--in", other, "--state", file("readers/state.json"), "--out", at("readers/state.json"), "--log-through", "10"}, at("readers/state.json"), "--out names the --state file"},
		{[]string{"readers", "--in", other, "--out", token, "--log-through", "10"}, token, "--out names the DISPATCH_TOKEN_FILE file"},
		{[]string{"fetch", "--through", "10", "--out", token}, token, "--out names the DISPATCH_TOKEN_FILE file"},
		{[]string{"group", "--in", file("group.jsonl"), "--out", at("group.jsonl")}, at("group.jsonl"), "--out names the --in file"},
		{[]string{"rule-cost", "--in", file("rule-cost/table.json"), "--seed", "1", "--out", at("rule-cost")}, at("rule-cost/table.json"), "table.json of --out is the --in file"},
		{[]string{"split", "--in", file("split/counts.json"), "--cut-a", "1", "--cut-b", "2", "--out", at("split")}, at("split/counts.json"), "counts.json of --out is the --in file"},
		{[]string{"plant", "--in", other, "--corpus", file("plant/planted.jsonl"), "--seed", "1", "--out", at("plant")}, at("plant/planted.jsonl"), "planted.jsonl of --out is the --corpus file"},
		{[]string{"validate-labeller", "--labels", file("validate/report.json"), "--planted", other, "--seed", "1", "--out", at("validate/report.md")}, at("validate/report.json"), "report.json of --out is the --labels file"},
		{[]string{"recall", "--labels", file("recall/recall.json"), "--out", at("recall")}, at("recall/recall.json"), "recall.json of --out is the --labels file"},
		{append([]string{"score", "--labels", file("score.jsonl"), "--stratum", stratumReal, "--out", at("score"), "--final-test", "--set-digest", "ab", "--seed", "1", "--exposure-log", at("score.jsonl"), "--model-id", "stub"}, reader...), at("score.jsonl"), "--exposure-log names the --labels file"},
		{append([]string{"regress", "--cases", file("regress.jsonl"), "--repeat-bound", "0.1", "--out", at("regress.jsonl")}, reader...), at("regress.jsonl"), "--out names the --cases file"},
		{append([]string{"reading", "--in", other, "--after", "1", "--through", "2", "--exclude", file("reading/report.json"), "--out", at("reading")}, reader...), at("reading/report.json"), "report.json of --out is the --exclude file"},
		{[]string{"calibrate", "--name", "n", "--rows", file("rows.jsonl"), "--pairs", other, "--measured", at("rows.jsonl")}, at("rows.jsonl"), "--measured names the --rows file"},
		{[]string{"dataset", "fetch", "--end", fixtureEnd, "--owner", fixtureOwner, "--out", token}, token, "--out names the DISPATCH_TOKEN_FILE file"},
		{[]string{"dataset", "fetch", "--end", fixtureEnd, "--owner", fixtureOwner, "--labelled", at("labelled"), "--out", at("labelled/snapshot.jsonl.gz")}, labelled, "the --out file lies in the --labelled directory"},
		{[]string{"dataset", "classify", "--snapshot", file("snapshot.jsonl.gz"), "--out", at("snapshot.jsonl.gz")}, at("snapshot.jsonl.gz"), "--out names the --snapshot file"},
		{[]string{"dataset", "build", "--snapshot", file("build/counts.json"), "--verdicts", other, "--seed", "1", "--out", at("build")}, at("build/counts.json"), "counts.json of --out is the --snapshot file"},
		{[]string{"dataset", "pack", "--snapshot", other, "--verdicts", file("pack/dataset.tar.gz.manifest.json"), "--seed", "1", "--out", at("pack/dataset.tar.gz")}, at("pack/dataset.tar.gz.manifest.json"), "dataset.tar.gz.manifest.json of --out is the --verdicts file"},
		{[]string{"dataset", "pack", "--snapshot", file("pack/dataset.tar.gz.part-000"), "--verdicts", other, "--seed", "1", "--out", at("pack/dataset.tar.gz")}, at("pack/dataset.tar.gz.part-000"), "dataset.tar.gz.part-000 of --out is the --snapshot file"},
		{[]string{"dataset", "sessions", "--manifest", file("sessions/places.jsonl"), "--cases", other, "--out", at("sessions")}, at("sessions/places.jsonl"), "places.jsonl of --out is the --manifest file"},
		{[]string{"dataset", "owner-texts", "--snapshot", file("owner/owner-texts.jsonl.gz"), "--out", at("owner")}, at("owner/owner-texts.jsonl.gz"), "owner-texts.jsonl.gz of --out is the --snapshot file"},
		{[]string{"dataset", "followups", "--from", other, "--cases", filepath.Dir(file("followups/explanations/cases.jsonl.gz")), "--out", at("followups")}, at("followups/explanations/cases.jsonl.gz"), "explanations of --out, which this command replaces whole, holds the --cases file"},
		{[]string{"dataset", "named", "--doc", "NAMED-2/notes-md@2", "--owner", fixtureOwner, "--out", at("named")}, token, "counts.json of --out is the DISPATCH_TOKEN_FILE file"},
		{[]string{"dataset", "lists", "--cases", other, "--verdicts", file("lists/question.txt"), "--plain-labels", other, "--out", at("lists")}, at("lists/question.txt"), "question.txt of --out is the --verdicts file"},
	} {
		code, _, stderr := runCmd(t, c.args...)
		if code != 2 || !strings.Contains(stderr, c.wants) {
			t.Errorf("%v = %d:\n%s\nwant 2 and %q", c.args, code, stderr, c.wants)
		}
		if data, err := os.ReadFile(c.input); err != nil || string(data) != "keep\n" {
			t.Errorf("%v changed its input %s to %q (%v)", c.args[:2], c.input, data, err)
		}
	}
}

// A command that writes more than one output refuses, with exit 2 and before it writes anything,
// two of them that are one file, also where the file does not exist yet: score --final-test's
// exposure log named as its figures.json, whose write would replace the claim; and dataset fetch's
// snapshot named inside its work directory, whose every page file the fetch reads, as a page of
// the directory or as a file of any other name (readers' --out and --state:
// TestReadersRefusesOnePathForItsTwoOutputs).
func TestEveryCommandRefusesTwoOutputsOnOnePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DISPATCH_URL", "http://127.0.0.1:1")
	t.Setenv("DISPATCH_TOKEN_FILE", filepath.Join(dir, "token"))
	labels, key := filepath.Join(dir, "labels.jsonl"), filepath.Join(dir, "key")
	for _, path := range []string{labels, key} {
		if err := os.WriteFile(path, []byte("keep\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reader := []string{"--kind", "openai-chat", "--endpoint", "http://127.0.0.1:1/", "--deployment", "stub", "--key-file", key}
	score, pages, named := filepath.Join(dir, "score"), filepath.Join(dir, "pages"), filepath.Join(dir, "named")
	for _, c := range []struct {
		args  []string
		made  string
		wants string
	}{
		{append([]string{"score", "--labels", labels, "--stratum", stratumReal, "--out", score, "--final-test", "--set-digest", "ab", "--seed", "1",
			"--exposure-log", filepath.Join(score, "figures.json"), "--model-id", "stub"}, reader...), score,
			"--exposure-log names figures.json of --out, which this command also writes"},
		{[]string{"dataset", "fetch", "--end", fixtureEnd, "--owner", fixtureOwner, "--out", filepath.Join(pages, "events-0-10.jsonl.gz"), "--work", pages}, pages,
			"the --out file lies in the --work directory, which this command reads whole"},
		{[]string{"dataset", "fetch", "--end", fixtureEnd, "--owner", fixtureOwner, "--out", filepath.Join(named, "snapshot.jsonl.gz"), "--work", named}, named,
			"the --out file lies in the --work directory, which this command reads whole"},
	} {
		code, _, stderr := runCmd(t, c.args...)
		if code != 2 || !strings.Contains(stderr, c.wants) {
			t.Errorf("%v = %d:\n%s\nwant 2 and %q", c.args, code, stderr, c.wants)
		}
		if _, err := os.Stat(c.made); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the refused %s made %s (%v)", c.args[0], c.made, err)
		}
	}
}

// Every other command of package main that makes its output directory makes the one it writes
// its files in, as score and reading do (TestAFinalTestMakesTheOutItWrites): --out
// work/link/../<name> is work/<name>, and the command makes no x/<name>. build and followups
// stage their files in a directory under --out (dsStaged), and build --from, owner-texts --from
// and followups join the bundle's files in one (dsMakeOut): each is made in work/<name> too, and
// removed once the command ends.
func TestEveryCommandMakesTheOutItWrites(t *testing.T) {
	work, x := linkedDirs(t)
	dir := t.TempDir()
	// lists: a build of one development question case, with no verdict and no label.
	listed := filepath.Join(dir, "listed")
	if err := os.Mkdir(listed, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := dsWriteGzipJSONL(filepath.Join(listed, "cases.jsonl.gz"), []dsListCase{{ID: "question/q1", Group: dsGroupQuestion, Part: dsDevelopment}}); err != nil {
		t.Fatal(err)
	}
	none := filepath.Join(dir, "none.jsonl")
	if err := os.WriteFile(none, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// plant, rule-cost and split: a grouped corpus of made-up writes.
	writes := syntheticCorpus(map[string]int{"comment": 20, "document": 20, "message": 20, "result": 20, "issue_title": 20})
	assignGroups(writes)
	corpus := writeCases(t, dir, "corpus.jsonl", writes)
	labels, manifest := fixtureLabels(t, dir)
	// build, owner-texts and followups: followups' fixture, built and packed.
	in := dsInputs{Seed: fixtureSeed}
	in.Snapshot, in.Verdicts = followupsFixture().write(t, dir)
	built, bundle := filepath.Join(dir, "build"), filepath.Join(dir, "bundle.tar.gz")
	if _, err := buildDataset(in, built); err != nil {
		t.Fatal(err)
	}
	if _, _, err := packBundle(in, bundle, dsPartSize); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		args  []string
		files []string
	}{
		{"lists", []string{"dataset", "lists", "--cases", listed, "--verdicts", none, "--plain-labels", none}, []string{dsListQuestion, dsListAnswer, dsListPlain}},
		{"plant", []string{"plant", "--in", corpus, "--corpus", corpus, "--seed", "11", "--per-shape", "6", "--nolist", "8", "--controls"}, []string{"planted.jsonl", "manifest.jsonl", "sha256"}},
		{"recall", []string{"recall", "--labels", labels, "--planted", manifest}, []string{"recall.json", "recall.md"}},
		{"rule-cost", []string{"rule-cost", "--in", corpus, "--seed", "3", "--writes", "50", "--min-per-surface", "10"}, []string{"table.md", "table.json", "manifest.jsonl", "manifest.sha256"}},
		{"split", []string{"split", "--in", corpus, "--cut-a", "1010", "--cut-b", "1030"}, []string{setDevelopment + ".jsonl", setCalibration + ".jsonl", setFinalPool + ".jsonl", "counts.json"}},
		{"build", []string{"dataset", "build", "--snapshot", in.Snapshot, "--verdicts", in.Verdicts, "--seed", strconv.Itoa(fixtureSeed)}, dsBuildFiles},
		{"build-from", []string{"dataset", "build", "--from", bundle}, dsBuildFiles},
		{"owner-texts", []string{"dataset", "owner-texts", "--from", bundle}, []string{dsOwnerTextsFile}},
		{"followups", []string{"dataset", "followups", "--from", bundle, "--cases", built}, []string{dsFollowupsFile, dsExplanationsDir}},
	} {
		// Written out, as linkedDirs says.
		if code, _, stderr := runCmd(t, append(c.args, "--out", work+"/link/../"+c.name)...); code != 0 {
			t.Errorf("%s --out work/link/../%s = %d:\n%s", strings.Join(c.args, " "), c.name, code, stderr)
		}
		if names, err := dirNames(filepath.Join(work, c.name)); err != nil || !slices.Equal(names, slices.Sorted(slices.Values(c.files))) {
			t.Errorf("work/%s holds %v (%v), want %v", c.name, names, err, c.files)
		}
		if _, err := os.Stat(filepath.Join(x, c.name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the %s run made x/%s (%v), a directory it writes nothing in", c.name, c.name, err)
		}
	}
}

// dirNames is the names of dir's entries, in order, or the error its listing gave.
func dirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, err
}

// A write renamed over its output once whole goes through a file of a name of its own: a file
// beside the output under the name the temporary file had before (<out>.tmp), which another run
// on the same output may be writing, is neither truncated, renamed nor removed, whether the write
// ends or is refused, and a refused write leaves no file of its own behind.
func TestAWriteBesideAnOutputLeavesAnotherRunsFileAlone(t *testing.T) {
	dir := t.TempDir()
	// another writes another run's file beside out, and checks it and that no other file of a
	// name beside out is left.
	another := func(out string) func(string) {
		if err := os.WriteFile(out+".tmp", []byte("another run's\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return func(what string) {
			t.Helper()
			if data, err := os.ReadFile(out + ".tmp"); err != nil || string(data) != "another run's\n" {
				t.Errorf("%s left another run's %s.tmp as %q (%v)", what, out, data, err)
			}
			if left, _ := filepath.Glob(out + ".*.tmp"); len(left) != 0 {
				t.Errorf("%s left %v", what, left)
			}
		}
	}
	labels := filepath.Join(dir, "labels.jsonl")
	check := another(labels)
	f, err := startLabels(labels, []labelRecord{{EventID: 7, SourceID: "c7"}})
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	check("label's start of the labels file")

	cases, out := filepath.Join(dir, "cases.jsonl"), filepath.Join(dir, "readers.jsonl")
	writeJSONLines(t, cases, Case{EventID: 99, SourceID: "c99", Surface: "comment", Field: "body", Text: "Late."})
	check = another(out)
	if _, err := writeReaders(cases, out, &replayState{Through: 10}, 0, 10); err == nil || !strings.Contains(err.Error(), "outside the replayed window") {
		t.Errorf("readers of a case outside the window: %v", err)
	}
	check("a refused readers")

	state := filepath.Join(dir, "state.json")
	check = another(state)
	if err := saveReplayState(state, &replayState{Through: 10}); err != nil {
		t.Fatal(err)
	}
	check("the replayed state's save")

	snapshot, verdicts := standardFixture().write(t, dir)
	bundle := filepath.Join(dir, "dataset.tar.gz")
	check = another(bundle)
	if _, _, err := packBundle(dsInputs{Snapshot: snapshot, Verdicts: verdicts, Seed: fixtureSeed}, bundle, dsPartSize); err != nil {
		t.Fatal(err)
	}
	check("pack")
}

// One fetch at a time writes fetch's cases files and dataset fetch's work directory: a second run
// on the same --out or --work while the first holds its lock, resumed or not, is refused with exit
// 2, naming the lock, before it writes anything, and the first run's files are left as they were.
func TestASecondFetchOnTheFilesOfARunningOneIsRefused(t *testing.T) {
	dir := t.TempDir()
	write := func(path, data string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unchanged := func(path, data string) {
		t.Helper()
		if got, err := os.ReadFile(path); err != nil || string(got) != data {
			t.Errorf("the refused run left %s as %q (%v), want %q", path, got, err, data)
		}
	}
	token := filepath.Join(dir, "token")
	write(token, "stand-in\n")
	t.Setenv("DISPATCH_URL", "http://127.0.0.1:1")
	t.Setenv("DISPATCH_TOKEN_FILE", token)
	hold := func(path string) {
		lock, held, err := harness.LockRun(path)
		if err != nil || held {
			t.Fatalf("the first run's lock %s: held %t, %v", path, held, err)
		}
		t.Cleanup(func() { lock.Close() })
	}

	out := filepath.Join(dir, "cases.jsonl")
	partial := out + ".partial"
	params, err := json.Marshal(fetchParams{DispatchURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	write(partial, "the first fetch's cases\n")
	write(fetchParamsPath(partial), string(params))
	hold(out + ".lock")
	for _, args := range [][]string{{"fetch", "--through", "10", "--out", out}, {"fetch", "--through", "10", "--out", out, "--resume"}} {
		if code, _, stderr := runCmd(t, args...); code != 2 || !strings.Contains(stderr, "another fetch holds "+out+".lock") {
			t.Errorf("%v while another fetch holds the lock = %d:\n%s", args, code, stderr)
		}
		unchanged(partial, "the first fetch's cases\n")
		unchanged(fetchParamsPath(partial), string(params))
	}

	work := filepath.Join(dir, "snapshot.work")
	write(filepath.Join(work, "bodies.jsonl"), "the first fetch's reads\n")
	hold(filepath.Join(work, "fetch.lock"))
	code, _, stderr := runCmd(t, "dataset", "fetch", "--end", fixtureEnd, "--owner", fixtureOwner, "--out", filepath.Join(dir, "snapshot.jsonl.gz"), "--work", work)
	if code != 2 || !strings.Contains(stderr, "another fetch holds "+filepath.Join(work, "fetch.lock")) {
		t.Errorf("dataset fetch while another fetch holds its work directory = %d:\n%s", code, stderr)
	}
	unchanged(filepath.Join(work, "bodies.jsonl"), "the first fetch's reads\n")
	if _, err := os.Stat(filepath.Join(work, "fetch.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused dataset fetch wrote %s (%v)", filepath.Join(work, "fetch.json"), err)
	}
}

// One label run at a time writes a labels file: a resume while another run holds its lock is
// refused with exit 2, naming the lock, before it renames a file over --out, so the file the first
// run appends its records to stays the labels file. The first run's record is one the resume
// would keep, so with no lock the resume would go on and replace the file.
func TestASecondLabelRunOnTheLabelsFileOfARunningOneIsRefused(t *testing.T) {
	dir := t.TempDir()
	text := "The fix names the risk."
	cases, definition, key := labelFixture(t, dir, text)
	rubric, err := os.ReadFile(definition)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := labelJobs([]Case{{EventID: 7, SourceID: "c7", Surface: "comment", Field: "body", Text: text}}, func(string, int64) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "labels.jsonl")
	writeJSONLines(t, out, labelRecord{EventID: 7, SourceID: "c7", Family: "comment", WriteLabel: writePass,
		Params: labelParams(labelPreamble+string(rubric), "claude-opus-5-5", "off", 60), Input: jobs[0].input,
		Sentences: []sentenceLabel{{ID: "s1", Field: "body", Text: text, Verdict: verdictNotVacuous, Shape: "none"}}, Calls: []labelCall{}})
	before, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	lock, held, err := harness.LockRun(out + ".lock")
	if err != nil || held {
		t.Fatalf("the first run's lock: held %t, %v", held, err)
	}
	defer lock.Close()
	stub := newStubModel(t, containsAny("names the"), nil)
	code, _, stderr := runCmd(t, labelArgs(stub, cases, definition, key, out, "--resume")...)
	if code != 2 || !strings.Contains(stderr, "another label run holds "+out+".lock") {
		t.Errorf("label --resume while another run holds the lock = %d:\n%s", code, stderr)
	}
	if after, err := os.Stat(out); err != nil || !os.SameFile(before, after) {
		t.Errorf("the refused run replaced %s (%v)", out, err)
	}
	if stub.calls.Load() != 0 {
		t.Error("the refused run called the model")
	}
}
