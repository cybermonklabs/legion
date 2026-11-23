package harness

// The harness's reuse of earlier results and its refusals: a run reuses earlier results only under
// the digests of every input that changes them, a second `harness local` on one --out is refused,
// a credential goes over clear text only inside the cluster, and the pacer charges a call as
// modelpace does (dispatch://INFRA-3231).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
	"github.com/sjawhar/envoy/internal/testwait"
)

func TestCredentialURLRefusesClearTextOutsideTheCluster(t *testing.T) {
	for _, c := range []struct {
		raw string
		ok  bool
	}{
		{"https://gateway.example.com", true},
		{"http://127.0.0.1:4000", true},
		{"http://[::1]:4000/", true},
		{"http://localhost:4000", true},
		{"http://model-gateway.legion.svc:80", true},
		{"http://model-gateway.legion.svc.cluster.local", true},
		{"", true},
		{"http://gateway.example.com", false},
		{"http://10.0.0.7:4000", false},
		{"ftp://gateway.example.com", false},
		{"gateway.example.com", false},
	} {
		err := CredentialURL("--gateway", c.raw)
		if (err == nil) != c.ok {
			t.Errorf("CredentialURL(%q) = %v, want ok %t", c.raw, err, c.ok)
		}
	}
	code, _, stderr := runHarness("probe", "--gateway", "http://gateway.example.com", "--gateway-key-file", keyFile(t))
	if code != 2 || !strings.Contains(stderr, "is http to a host outside the cluster") {
		t.Errorf("harness probe at an http gateway outside the cluster = %d:\n%s", code, stderr)
	}
}

// WriterDigest names the refusal lines change 2's writer reads before the rules.
func TestWriterDigestChangesWithTheRefusalLines(t *testing.T) {
	before := WriterDigest()
	lines := writingRulesRefusalLines
	t.Cleanup(func() { writingRulesRefusalLines = lines })
	writingRulesRefusalLines = append(append([]string{}, lines...), "4. A line more.")
	if WriterDigest() == before {
		t.Error("WriterDigest is the same with a refusal line more")
	}
}

// The reviewer's digest moves with a prompt of the reviewer the instance is built from, and not
// with a test file, and the settings a change's run records hold it.
func TestTheReviewerDigestMovesWithAReviewerPrompt(t *testing.T) {
	if _, err := reviewerDigest(t.TempDir()); err == nil {
		t.Error("a root with no reviewer has a digest")
	}
	root := reviewerRoot(t)
	dir := filepath.Join(root, filepath.FromSlash(reviewerDir))
	before, err := reviewerDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rules_test.go"), []byte("package reviewer // a test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if same, _ := reviewerDigest(root); same != before {
		t.Error("a test file moved the reviewer's digest")
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "rules.md"), []byte("Name the fact, and its owner.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := reviewerDigest(root)
	if err != nil || after == before {
		t.Errorf("a changed prompt gave %s (%v), the digest before it", after, err)
	}
	if got := (LocalInstance{ReviewerSHA256: after}).readSettings()["reviewer_sha256"]; got != after {
		t.Errorf("the settings a run records hold reviewer_sha256 %q, want %q", got, after)
	}
}

// The reviewer's digest moves with a file of the plain-words skill its rule sets and the written
// rules are cut from, and with a file of the writing check's bundle its detector rule set runs:
// each changes what the reviewer finds.
func TestTheReviewerDigestMovesWithThePlainWordsSkillAndTheBundle(t *testing.T) {
	root := reviewerRoot(t)
	before, err := reviewerDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		plainWordsDir + "/skill/SKILL.md",
		plainWordsDir + "/skill/references/patterns.md",
		"packages/envoy/internal/dispatch/writingcheck/reader/prompt.md",
	} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte("Changed.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		after, err := reviewerDigest(root)
		if err != nil || after == before {
			t.Errorf("a changed %s gave %s (%v), the digest before it", name, after, err)
		}
		before = after
	}
}

// reviewerRoot is a repository root that holds one file of each part reviewerDigest reads: the
// reviewer's code, a test of it and a prompt, package plainwords and its skill, and each part of
// the writing check's bundle.
func reviewerRoot(t *testing.T) string {
	t.Helper()
	files := map[string]string{
		reviewerDir + "/rules.go":                       "package reviewer\n",
		reviewerDir + "/rules_test.go":                  "package reviewer\n",
		reviewerDir + "/prompts/rules.md":               "Name the fact.\n",
		plainWordsDir + "/plainwords.go":                "package plainwords\n",
		plainWordsDir + "/skill/SKILL.md":               "Say what changed.\n",
		plainWordsDir + "/skill/references/patterns.md": "Name the owner.\n",
	}
	for _, part := range bundleParts {
		names := part.files
		if names == nil {
			names = []string{"part.go"}
		}
		for _, name := range append(slices.Clone(names), part.extra...) {
			files["packages/envoy/"+part.dir+"/"+name] = "package part\n"
		}
	}
	root := t.TempDir()
	for name, data := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// phrasesCases and phrasesLabels are a labeller round's cases.jsonl and labels.jsonl of one write.
const (
	phrasesCases  = `{"event_id":1,"source_id":"a","field":"body","group":"g1","text":"This names the problem. The run is green."}` + "\n"
	phrasesLabels = `{"event_id":1,"source_id":"a","family":"comment","sentences":[{"id":"s1","field":"body","text":"This names the problem.","verdict":"vacuous"},{"id":"s2","field":"body","text":"The run is green.","verdict":"not_vacuous"}]}` + "\n"
)

// writePhrasesRound writes phrasesCases and phrasesLabels in dir.
func writePhrasesRound(t *testing.T, dir string) {
	t.Helper()
	for name, data := range map[string]string{"cases.jsonl": phrasesCases, "labels.jsonl": phrasesLabels} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// phrases-labels reuses its results only for the labels' bytes they were graded from: labels
// changed in place, in the same directory, are refused there.
func TestPhrasesLabelsRefusesLabelsChangedInPlace(t *testing.T) {
	dir := t.TempDir()
	writePhrasesRound(t, dir)
	stand := modeltest.New(t, graderRules(t)...)
	out := t.TempDir()
	args := []string{"phrases-labels", "--labels", dir, "--out", out, "--gateway", stand.URL, "--gateway-key-file", keyFile(t), "--draws", "50"}
	if code, _, stderr := runHarness(args...); code != 0 {
		t.Fatalf("phrases-labels = %d: %s", code, stderr)
	}
	calls := len(stand.Requests())
	relabelled := strings.Replace(phrasesLabels, `"verdict":"not_vacuous"`, `"verdict":"vacuous"`, 1)
	if err := os.WriteFile(filepath.Join(dir, "labels.jsonl"), []byte(relabelled), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runHarness(args...)
	if code != 1 || !strings.Contains(stderr, "holds a run of other parameters") || !strings.Contains(stderr, "labels_sha256") {
		t.Errorf("phrases-labels of labels changed in place = %d: %s", code, stderr)
	}
	if n := len(stand.Requests()); n != calls {
		t.Errorf("the refused run made %d calls", n-calls)
	}
}

// phrases-labels reuses its results only for the labels directory they were graded from, where
// its files are read: with work/link pointing at x/sub, ../labels from work/link is x/labels, a
// directory of its own though it holds the same bytes, and though --labels made absolute and
// clean is work/labels, the first run's.
func TestPhrasesLabelsRefusesTheLabelsOfAnotherDirectory(t *testing.T) {
	work, x := linkedDirs(t)
	for _, d := range []string{filepath.Join(work, "labels"), filepath.Join(x, "labels")} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writePhrasesRound(t, filepath.Join(work, "labels"))
	writePhrasesRound(t, filepath.Join(x, "labels"))
	stand := modeltest.New(t, graderRules(t)...)
	out, key := t.TempDir(), keyFile(t)
	phrases := func(labels string) (int, string) {
		code, _, stderr := runHarness("phrases-labels", "--labels", labels, "--out", out, "--gateway", stand.URL, "--gateway-key-file", key, "--draws", "50")
		return code, stderr
	}
	if code, stderr := phrases(filepath.Join(work, "labels")); code != 0 {
		t.Fatalf("phrases-labels = %d: %s", code, stderr)
	}
	calls := len(stand.Requests())
	// t.Chdir sets PWD to work/link, the name os.Getwd answers.
	t.Chdir(filepath.Join(work, "link"))
	code, stderr := phrases("../labels")
	if want := `"labels":"` + filepath.Join(x, "labels") + `"`; code != 1 || !strings.Contains(stderr, "holds a run of other parameters") || !strings.Contains(stderr, want) {
		t.Errorf("phrases-labels of x/labels in work/labels's --out = %d, want 1 and the refusal naming %s: %s", code, want, stderr)
	}
	if n := len(stand.Requests()); n != calls {
		t.Errorf("the refused run made %d calls", n-calls)
	}
}

// A run of change 7a records the digest of the bundle it runs in process, and a run that
// continues its directory under another bundle is refused.
func TestASevenARunRecordsItsBundle(t *testing.T) {
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, lockstepRules(t, nil)...)
	out := t.TempDir()
	if code, stderr := scoreSevenA(t, stand, build, sessions, out, "--model", laneA); code != 0 {
		t.Fatalf("the 7a run = %d:\n%s", code, stderr)
	}
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := BundleDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(out, "run.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var params RunParams
	if err := json.Unmarshal(data, &params); err != nil || params.CheckBundleSHA256 != want {
		t.Fatalf("run.json records check_bundle_sha256 %q (%v), want the bundle's %s", params.CheckBundleSHA256, err, want)
	}
	params.CheckBundleSHA256 = strings.Repeat("0", 64)
	writeJSONFile(t, path, params)
	if code, stderr := scoreSevenA(t, stand, build, sessions, out, "--model", laneA); code != 1 || !strings.Contains(stderr, "holds a run of other parameters") {
		t.Errorf("a 7a run continuing a directory of another bundle = %d:\n%s", code, stderr)
	}
}

// stubBun puts first on PATH a `bun` that writes its pid in the file it answers and sleeps, so a
// `harness local` start runs an instance that never comes up; as the test ends it kills the
// process group of each stub that wrote its pid there (the start runs bun in a group of its own).
func stubBun(t *testing.T) (pids string) {
	t.Helper()
	bin := t.TempDir()
	pids = filepath.Join(t.TempDir(), "pids")
	script := "#!/bin/sh\necho $$ >> \"$HARNESS_TEST_BUN_PID\"\nexec sleep 300\n"
	if err := os.WriteFile(filepath.Join(bin, "bun"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HARNESS_TEST_BUN_PID", pids)
	t.Cleanup(func() {
		data, _ := os.ReadFile(pids)
		for _, line := range strings.Fields(string(data)) {
			if pid, err := strconv.Atoi(line); err == nil {
				syscall.Kill(-pid, syscall.SIGKILL)
			}
		}
	})
	return pids
}

// A second `harness local` on the --out a first start holds is refused at once, even before the
// first instance is up; and a start removes, when it ends, only the local.json it made, not a
// file another start wrote there since.
func TestASecondLocalStartOnOneOutIsRefused(t *testing.T) {
	out := t.TempDir()
	pids := stubBun(t)
	args := []string{"local", "--out", out, "--pg-dsn", "postgres://harness@127.0.0.1:1/postgres", "--database", "dispatch_harness_1", "--repo", t.TempDir()}
	first, stopFirst := context.WithCancel(context.Background())
	defer stopFirst()
	firstDone := make(chan int, 1)
	go func() {
		var stdout, stderr strings.Builder
		firstDone <- Run(first, args, &stdout, &stderr)
	}()
	testwait.Eventually(t, "the first start's instance to run", func() bool {
		data, _ := os.ReadFile(pids)
		return len(strings.Fields(string(data))) == 1
	})
	// The bound ends a second start that was let through; one refused returns at once.
	second, stopSecond := context.WithTimeout(context.Background(), 20*time.Second)
	defer stopSecond()
	var stdout, stderr strings.Builder
	if code := Run(second, args, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "another `harness local` holds this --out") {
		t.Errorf("a second start on the held --out = %d:\n%s", code, stderr.String())
	}
	// Another start's local.json takes the place of the first's: the first leaves it as it ends.
	held := filepath.Join(out, "local.json")
	if err := os.Remove(held); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(held, []byte(`{"base_url":"http://127.0.0.1:4200"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stopFirst()
	<-firstDone
	if data, err := os.ReadFile(held); err != nil || string(data) != `{"base_url":"http://127.0.0.1:4200"}` {
		t.Errorf("the first start, as it ended, removed or changed the local.json another wrote (%q, %v)", data, err)
	}
}

// A `harness local` start stopped (Ctrl-C) before its instance is up fails: it started no
// instance, as it was asked to. A stop once the instance is up is how it ends.
func TestALocalStartStoppedBeforeItsInstanceIsUpFails(t *testing.T) {
	out := t.TempDir()
	pids := stubBun(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan int, 1)
	var stderr strings.Builder
	go func() {
		var stdout strings.Builder
		done <- Run(ctx, []string{"local", "--out", out, "--pg-dsn", "postgres://harness@127.0.0.1:1/postgres", "--database", "dispatch_harness_1", "--repo", t.TempDir()}, &stdout, &stderr)
	}()
	testwait.Eventually(t, "the start's instance to run", func() bool {
		data, _ := os.ReadFile(pids)
		return len(strings.Fields(string(data))) == 1
	})
	stop()
	if code := <-done; code != 1 || !strings.Contains(stderr.String(), "before the instance was up") {
		t.Errorf("a start stopped before its instance was up = %d:\n%s", code, stderr.String())
	}
}

// Every harness command refuses, with exit 2 and before it writes anything, an output that is one
// of its inputs: a file it writes, or one it writes in its output directory, that is a file
// another of its flags reads (a key file, --case-list, the build's cases, --planted, a run's
// files, local.json). The input is left as it was.
func TestEveryHarnessCommandRefusesAnOutputThatIsAnInput(t *testing.T) {
	build, sessions := writeFixture(t)
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
	key := file("key")
	// The gateway and Postgres are a port that answers nothing, so a command that went on past
	// its refusal fails at once.
	gateway := []string{"--gateway", "http://127.0.0.1:1", "--gateway-key-file", key}
	builtCases := filepath.Join(build, casesFile)
	for _, c := range []struct {
		args         []string
		input, wants string
	}{
		{append([]string{"probe", "--out", key}, gateway...), key, "--out names the --gateway-key-file file"},
		{append([]string{"score", "--cases", build, "--sessions", sessions, "--out", at("score"), "--case-list", file("score/scores.md")}, gateway...), at("score/scores.md"), "scores.md of --out is the --case-list file"},
		{append([]string{"score", "--part", PartHeldOut, "--commit", "c0ffee", "--cases", build, "--sessions", sessions, "--out", at("held"), "--exposure-log", builtCases}, gateway...), builtCases, "--exposure-log names cases.jsonl.gz of --cases"},
		{append([]string{"grader-test", "--planted", file("grader/grader-test.json"), "--cases", build, "--sessions", sessions, "--out", at("grader")}, gateway...), at("grader/grader-test.json"), "grader-test.json of --out is the --planted file"},
		{[]string{"compare", "--a", filepath.Dir(file("a/run.json")), "--b", at("a"), "--out", at("a/run.json")}, at("a/run.json"), "--out names run.json of --a"},
		{[]string{"phrases-labels", "--labels", at("labels"), "--out", at("phrases"), "--gateway-key-file", file("phrases/results.jsonl")}, at("phrases/results.jsonl"), "results.jsonl of --out is the --gateway-key-file file"},
		{[]string{"asked-words", "--run", filepath.Dir(file("run/cases.jsonl")), "--cases", build, "--out", at("run/cases.jsonl")}, at("run/cases.jsonl"), "--out names cases.jsonl of --run"},
		{[]string{"local", "--out", at("local"), "--pg-dsn", "postgres://127.0.0.1:1/x", "--gateway-key-file", file("local/local.json")}, at("local/local.json"), "local.json of --out is the --gateway-key-file file"},
		{[]string{"load", "--local", file("load/loaded.jsonl"), "--cases", build, "--verdicts", key, "--out", at("load")}, at("load/loaded.jsonl"), "loaded.jsonl of --out is the --local file"},
	} {
		before, err := os.ReadFile(c.input)
		if err != nil {
			t.Fatal(err)
		}
		code, _, stderr := runHarness(c.args...)
		if code != 2 || !strings.Contains(stderr, c.wants) {
			t.Errorf("harness %v = %d:\n%s\nwant 2 and %q", c.args, code, stderr, c.wants)
		}
		if after, err := os.ReadFile(c.input); err != nil || string(after) != string(before) {
			t.Errorf("harness %s changed its input %s (%v)", c.args[0], c.input, err)
		}
	}
}

// A held-out score writes two outputs, its run directory and the exposure log, and refuses, with
// exit 2 and before it writes or claims anything, an exposure log that is a file of its run
// directory, also where neither exists yet: the run's write would replace its claim.
func TestAHeldOutScoreRefusesAnExposureLogInItsRunDirectory(t *testing.T) {
	build, sessions := writeFixture(t)
	out := filepath.Join(t.TempDir(), "held")
	gateway := []string{"--gateway", "http://127.0.0.1:1", "--gateway-key-file", keyFile(t)}
	code, _, stderr := runHarness(append([]string{"score", "--part", PartHeldOut, "--commit", "c0ffee", "--cases", build, "--sessions", sessions,
		"--out", out, "--exposure-log", filepath.Join(out, "run.json")}, gateway...)...)
	if want := "--exposure-log names run.json of --out, which this command also writes"; code != 2 || !strings.Contains(stderr, want) {
		t.Errorf("a held-out score whose exposure log is its run.json = %d:\n%s\nwant 2 and %q", code, stderr, want)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("the refused score made %s (%v)", out, err)
	}
}

// linkedDirs makes work and x/sub in a directory of the test's own, its links resolved, and
// work/link pointing at x/sub: the kernel takes work/link/.. to x, and filepath.Join cleans it to
// work. A test writes such a name out, not with filepath.Join, which would take the ".." away.
func linkedDirs(t *testing.T) (work, x string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work, x = filepath.Join(dir, "work"), filepath.Join(dir, "x")
	for _, d := range []string{work, filepath.Join(x, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(x, "sub"), filepath.Join(work, "link")); err != nil {
		t.Fatal(err)
	}
	return work, x
}

// NoOutputIsAnInput takes a name through a link and then ".." where the kernel takes it: to the
// parent of the link's target. With work/link pointing at x/sub, work/link/../in.json is
// x/in.json, an input no work directory the command replaces holds, and a new
// work/link/../out.json lies in an x the command reads whole; from work, the new
// link/../state.json is x/state.json, one file with an output of that path. A directory read
// whole is also where the command names its files by filepath.Join, which takes the ".." away
// with the link: the files of work/link/.. are work's, so a new work/page.json lies in it.
func TestNoOutputIsAnInputTakesADotDotAfterALinkAsTheKernelDoes(t *testing.T) {
	work, x := linkedDirs(t)
	if err := os.WriteFile(filepath.Join(x, "in.json"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Written out, as linkedDirs says.
	through := work + "/link/../"
	if err := NoOutputIsAnInput([]File{{Flag: "--out", Path: work, Tree: true}}, Files("--in", through+"in.json")); err != nil {
		t.Errorf("work/link/../in.json, which is x/in.json, refused as a file of work: %v", err)
	}
	err := NoOutputIsAnInput(Files("--out", through+"out.json"), []File{{Flag: "--in", Path: x, Tree: true}})
	if want := "the --out file lies in the --in directory, which this command reads whole"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a new work/link/../out.json with x read whole: %v, want %q", err, want)
	}
	err = NoOutputIsAnInput(Files("--out", filepath.Join(work, "page.json")), []File{{Flag: "--in", Path: through, Tree: true}})
	if want := "the --out file lies in the --in directory, which this command reads whole"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a new work/page.json with work/link/.. read whole: %v, want %q", err, want)
	}
	t.Chdir(work)
	err = NoOutputIsAnInput(append(Files("--out", filepath.Join(x, "state.json")), Files("--state", "link/../state.json")...), nil)
	if want := "--state names the --out file, which this command also writes"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a new link/../state.json from work and x/state.json: %v, want %q", err, want)
	}
}

// writeJSON, and each write ReplaceFile makes, goes through a file of a name of its own beside its
// output: another run's file under the name the temporary file had before (<path>.tmp) is left
// as it was, and a write that fails leaves no file of its own behind.
func TestReplaceFileLeavesAnotherRunsFileAlone(t *testing.T) {
	// CreateBeside names its file in the directory with its links resolved (t.TempDir is under a
	// link on macOS).
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "run.json")
	if err := os.WriteFile(path+".tmp", []byte("another run's\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, map[string]int{"cases": 1}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "{\n  \"cases\": 1\n}\n" {
		t.Errorf("writeJSON wrote %q (%v)", data, err)
	}
	if data, err := os.ReadFile(path + ".tmp"); err != nil || string(data) != "another run's\n" {
		t.Errorf("writeJSON left another run's run.json.tmp as %q (%v)", data, err)
	}
	// A rename over a directory that holds a file fails.
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(blocked, []byte("x"), 0o600); err == nil {
		t.Error("a write over a directory that holds a file answered no error")
	}
	for _, own := range []string{path + ".*.tmp", blocked + ".*.tmp"} {
		if left, _ := filepath.Glob(own); len(left) != 0 {
			t.Errorf("the writes left %v", left)
		}
	}
	a, err := CreateBeside(path, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(a.Name())
	defer a.Close()
	b, err := CreateBeside(path, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(b.Name())
	defer b.Close()
	if info, err := a.Stat(); a.Name() == b.Name() || filepath.Dir(a.Name()) != dir || err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("two files beside %s: %s and %s (%v)", path, a.Name(), b.Name(), err)
	}
}

// CreateBeside makes its file in the directory the rename over path lands in: work/link/../out.json
// lands in x, so the file is made in x, and not in work, from where the rename could cross a file
// system. A last name that is a link is the name the rename replaces, so the file is made beside
// the link, in work.
func TestCreateBesideMakesItsFileWhereTheRenameLands(t *testing.T) {
	work, x := linkedDirs(t)
	if err := os.Symlink(filepath.Join(x, "target.json"), filepath.Join(work, "last.json")); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{work + "/link/../out.json": x, filepath.Join(work, "last.json"): work} {
		f, err := CreateBeside(path, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		os.Remove(f.Name())
		if got := filepath.Dir(f.Name()); got != want {
			t.Errorf("CreateBeside(%s) made %s, want it in %s", path, f.Name(), want)
		}
	}
	if err := ReplaceFile(work+"/link/../out.json", []byte("whole\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(x, "out.json")); err != nil || string(data) != "whole\n" {
		t.Errorf("x/out.json holds %q (%v), want the write", data, err)
	}
}

// Every harness command that makes its output directory makes the one it writes its files in:
// --out work/link/../<name> is work/<name>, the name filepath.Join gives each file, and not
// x/<name>, where the kernel takes the name; so the command writes its files there and makes no
// x/<name>. load writes loaded.jsonl before it reaches the instance's database, here a port that
// answers nothing, and fails there. local: TestALocalStartMakesTheOutItWrites.
func TestEveryHarnessCommandMakesTheOutItWrites(t *testing.T) {
	work, x := linkedDirs(t)
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, allJudgementRules(t)...)
	gateway := []string{"--gateway", stand.URL, "--gateway-key-file", keyFile(t)}
	labels, dir := t.TempDir(), t.TempDir()
	writePhrasesRound(t, labels)
	local, verdicts := filepath.Join(dir, "local.json"), filepath.Join(dir, "verdicts.jsonl")
	writeJSONFile(t, local, LocalInstance{BaseURL: "http://127.0.0.1:4200", Database: "dispatch_harness_1", DatabaseURL: "postgres://127.0.0.1:1/dispatch_harness_1"})
	if err := os.WriteFile(verdicts, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		args []string
		// fails is what the command's failure says once it has written its files; "" for none.
		fails string
		files []string
	}{
		{"score", append([]string{"score", "--cases", build, "--sessions", sessions, "--draws", "50", "--exposure-log", filepath.Join(dir, "exposure.log")}, gateway...), "",
			[]string{"cases.jsonl", "run.json", "scores.json", "scores.md", "session-excerpts.jsonl.gz"}},
		{"phrases", append([]string{"phrases-labels", "--labels", labels, "--draws", "50"}, gateway...), "", []string{"agreement.json", "agreement.md", "results.jsonl", "run.json"}},
		{"grader", append([]string{"grader-test", "--planted", writePlanted(t), "--cases", build, "--sessions", sessions}, gateway...), "",
			[]string{"grader-test.json", "grader-test.md", "session-excerpts.jsonl.gz"}},
		{"load", []string{"load", "--local", local, "--cases", build, "--verdicts", verdicts}, "the local instance's database", []string{"loaded.jsonl"}},
	} {
		// Written out, as linkedDirs says.
		code, _, stderr := runHarness(append(c.args, "--out", work+"/link/../"+c.name)...)
		if c.fails == "" && code != 0 || c.fails != "" && (code != 1 || !strings.Contains(stderr, c.fails)) {
			t.Errorf("harness %s --out work/link/../%s = %d:\n%s", strings.Join(c.args, " "), c.name, code, stderr)
		}
		entries, err := os.ReadDir(filepath.Join(work, c.name))
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if err != nil || !slices.Equal(names, c.files) {
			t.Errorf("work/%s holds %v (%v), want %v", c.name, names, err, c.files)
		}
		if _, err := os.Stat(filepath.Join(x, c.name)); !os.IsNotExist(err) {
			t.Errorf("harness %s made x/%s (%v), a directory it writes nothing in", c.args[0], c.name, err)
		}
	}
}

// A `harness local` start makes the --out it writes local.json in, as the other harness commands
// do (TestEveryHarnessCommandMakesTheOutItWrites): --out work/link/../local is work/local, and the
// start makes no x/local.
func TestALocalStartMakesTheOutItWrites(t *testing.T) {
	work, x := linkedDirs(t)
	pids := stubBun(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	// Written out, as linkedDirs says.
	args := []string{"local", "--out", work + "/link/../local", "--pg-dsn", "postgres://harness@127.0.0.1:1/postgres", "--database", "dispatch_harness_1", "--repo", t.TempDir()}
	done := make(chan int, 1)
	var stderr strings.Builder
	go func() {
		var stdout strings.Builder
		done <- Run(ctx, args, &stdout, &stderr)
	}()
	testwait.Eventually(t, "the start's instance to run, or the start to end", func() bool {
		data, _ := os.ReadFile(pids)
		return len(strings.Fields(string(data))) == 1 || len(done) == 1
	})
	// The start removes its local.json as it ends.
	_, held := os.Stat(filepath.Join(work, "local", "local.json"))
	stop()
	if code := <-done; held != nil {
		t.Errorf("the start = %d, and wrote no work/local/local.json while its instance ran (%v):\n%s", code, held, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(x, "local")); !os.IsNotExist(err) {
		t.Errorf("the start made x/local (%v), a directory it writes nothing in", err)
	}
}
