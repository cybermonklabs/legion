package leakguard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// The tests of a gh call read one field at a time: each of them drives the fixture's two-word
// rule, `<name>[^\n]*<word>`, over a call whose two words stand in two fields GitHub stores apart,
// which passes, and in one field, which is refused. Each joined form is refused by the rule
// itself (refusedWhole), so a pass is the reader's and not a rule that never matched.

// ghFieldCall is the error GhCall answers req with, under a guard whose destination is public: a
// shared check that clears the call, and one that asks for its standard input first where the
// call has one.
func ghFieldCall(t *testing.T, req GhRequest) error {
	t.Helper()
	check := passes
	if req.Stdin != nil {
		check = leakguardtest.Reading(0, "")
	}
	req.Repo = "invented-org/invented-repo"
	_, err := guardWith(t, leakguardtest.Write(t, t.TempDir(), check), Public).GhCall(bound(t), req)
	return err
}

// stdinParse is the GhSent a caller's parse hands for a call whose one option of standard input
// is option, named with use (field, the name of the field the input is the value of, "" for any
// other option): what cmd/legion's ghSentFiles builds for it, by the rule of GhSent.Names.
func stdinParse(option, field string, use StdinUse) *GhSent {
	sent := &GhSent{}
	sent.Names(option, "-", field, use)
	return sent
}

// assetParse is the GhSent a caller's parse hands for a call that uploads paths as release
// assets, each written whole in the argv so that no label stands outside it: what cmd/legion's
// ghSentFiles builds for them, by the rule of GhSent.Asset.
func assetParse(paths ...string) *GhSent {
	sent := &GhSent{}
	for _, path := range paths {
		sent.Asset(path, "")
	}
	return sent
}

// wordBehindAnEscape is a one-line JSON document whose string `.a` holds the fixture's one word
// with its t written as a JSON escape, so that no rule reads the word in the document's bytes and
// only a read of its decoded strings finds it.
func wordBehindAnEscape(t *testing.T) string {
	t.Helper()
	document := `{"a":"the ` + strings.Replace(leakguardtest.Word, "t", `\u0074`, 1) + ` notes"}`
	if strings.Contains(document, leakguardtest.Word) {
		t.Fatalf("the document %q spells the word in its own bytes, so it proves nothing", document)
	}
	return document
}

// cleared fails the test unless err is nil.
func cleared(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("the call was refused: %v", err)
	}
}

// refusedWhole fails the test unless the fixture's rules refuse text read as one record: the
// joined form of a write whose fields the guard reads apart.
func refusedWhole(t *testing.T, text string) {
	t.Helper()
	if err := fixtureRules(t).Check(bound(t), "the joined text", Record{Label: "the joined text", Text: text}); err == nil {
		t.Fatalf("the rules clear %q read whole, so a pass of its fields proves nothing", text)
	}
}

// fieldFile writes text to a file of the test's own named name, and answers its path.
func fieldFile(t *testing.T, name, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// reviewDocument is a pull request review as `gh api --input` sends one: compact, on one line,
// with body the review's body and comment the body of its one comment.
func reviewDocument(t *testing.T, body, comment string) string {
	t.Helper()
	type reviewComment struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Body string `json:"body"`
	}
	document, err := json.Marshal(struct {
		Body     string          `json:"body"`
		Event    string          `json:"event"`
		Comments []reviewComment `json:"comments"`
	}{Body: body, Event: "COMMENT", Comments: []reviewComment{{Path: "main.go", Line: 19, Body: comment}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(document)
}

// A JSON body sent to GitHub's REST API is read one string at a time, as the API stores it: the
// fixture's two words in two fields of one compact review, which a raw read joins on one line,
// pass; the same two words in one field are refused, and the report names the field by its JSON
// path. A duplicate key's first value is read as well as its last. A text that is not exactly one
// document is read raw, and standard input named by an option other than --input is text GitHub
// stores as it stands. A body sent to another host is read whole
// (TestAnUploadsBodyIsReadAsGitHubStoresIt).
func TestGhJSONReadsOneFieldAtATime(t *testing.T) {
	const endpoint = "repos/invented-org/invented-repo/pulls/1/reviews"
	apart := reviewDocument(t, "the "+leakguardtest.PairName+" notes", "a "+leakguardtest.PairWord+" aside")
	together := reviewDocument(t, "a clean review", "the "+leakguardtest.PairName+" and the "+leakguardtest.PairWord)
	refusedWhole(t, apart)
	if strings.Contains(apart, "\n") {
		t.Fatalf("the review %q is on more than one line, so a raw read of it would not join its fields", apart)
	}
	t.Run("two fields of --input <file> pass", func(t *testing.T) {
		body := fieldFile(t, "review.json", apart)
		cleared(t, ghFieldCall(t, GhRequest{Args: []string{"api", endpoint, "--input", body}}))
	})
	t.Run("two fields of --input - pass", func(t *testing.T) {
		cleared(t, ghFieldCall(t, GhRequest{Args: []string{"api", endpoint, "--input", "-"}, Stdin: strings.NewReader(apart)}))
	})
	t.Run("two fields of standard input the caller names --input pass", func(t *testing.T) {
		cleared(t, ghFieldCall(t, GhRequest{
			Args:  []string{"api", endpoint, "--input", "-"},
			Stdin: strings.NewReader(apart),
			Sent:  stdinParse("--input", "", StdinRequestBody),
		}))
	})
	t.Run("one field of --input <file> is refused", func(t *testing.T) {
		body := fieldFile(t, "review.json", together)
		err := ghFieldCall(t, GhRequest{Args: []string{"api", endpoint, "--input", body}})
		reports(t, refusal(t, err).Lines, body+", JSON .comments[0].body: "+leakguardtest.PairClass)
	})
	t.Run("one field of --input - is refused", func(t *testing.T) {
		err := ghFieldCall(t, GhRequest{Args: []string{"api", endpoint, "--input", "-"}, Stdin: strings.NewReader(together)})
		reports(t, refusal(t, err).Lines, "the gh call's standard input, JSON .comments[0].body: "+leakguardtest.PairClass)
	})
	t.Run("a duplicate key's first value is read", func(t *testing.T) {
		body := fieldFile(t, "review.json", `{"body":"the `+leakguardtest.PairName+` and the `+leakguardtest.PairWord+`","body":"a clean review"}`)
		err := ghFieldCall(t, GhRequest{Args: []string{"api", endpoint, "--input", body}})
		reports(t, refusal(t, err).Lines, body+", JSON .body: "+leakguardtest.PairClass)
	})
	t.Run("a name is read as well as a value", func(t *testing.T) {
		body := fieldFile(t, "gist.json", `{"files":{"the `+leakguardtest.PairName+` and the `+leakguardtest.PairWord+`.md":{"content":"a clean file"}}}`)
		err := ghFieldCall(t, GhRequest{Args: []string{"api", "gists", "--input", body}})
		reports(t, refusal(t, err).Lines, body+`, JSON .files."the `+leakguardtest.PairName+` and the `+leakguardtest.PairWord+`.md" (name): `+leakguardtest.PairClass)
	})
	t.Run("a body of two documents is read raw", func(t *testing.T) {
		body := fieldFile(t, "review.json", `{"body":"the `+leakguardtest.PairName+` notes"} {"body":"a `+leakguardtest.PairWord+` aside"}`)
		err := ghFieldCall(t, GhRequest{Args: []string{"api", endpoint, "--input", body}})
		reports(t, refusal(t, err).Lines, body+": "+leakguardtest.PairClass)
	})
	t.Run("a Markdown body is read raw", func(t *testing.T) {
		markdown := "# Review\n\nthe " + leakguardtest.PairName + " and the " + leakguardtest.PairWord + "\n"
		err := ghFieldCall(t, GhRequest{Args: []string{"api", endpoint, "--input", "-"}, Stdin: strings.NewReader(markdown)})
		reports(t, refusal(t, err).Lines, "the gh call's standard input: "+leakguardtest.PairClass)
	})
	// A comment's body given on standard input is the comment GitHub shows, JSON or not, so the
	// two fields of a document given that way stand on its one line.
	t.Run("standard input the argv names --body-file - is read as text", func(t *testing.T) {
		err := ghFieldCall(t, GhRequest{Args: []string{"pr", "comment", "1", "--body-file", "-"}, Stdin: strings.NewReader(apart)})
		reports(t, refusal(t, err).Lines, "the gh call's standard input: "+leakguardtest.PairClass)
	})
	t.Run("standard input the caller names --body-file is read as text", func(t *testing.T) {
		err := ghFieldCall(t, GhRequest{
			Args:  []string{"pr", "comment", "1", "--body-file", "-"},
			Stdin: strings.NewReader(apart),
			Sent:  stdinParse("--body-file", "", StdinText),
		})
		reports(t, refusal(t, err).Lines, "the gh call's standard input: "+leakguardtest.PairClass)
	})
	// A caller that parsed the call but names no option of its standard input knows nothing of
	// it, and the table is not consulted for it: the input is read both ways.
	t.Run("standard input nothing names is read raw", func(t *testing.T) {
		err := ghFieldCall(t, GhRequest{Args: []string{"api", endpoint, "--input", "-"}, Stdin: strings.NewReader(apart), Sent: &GhSent{}})
		reports(t, refusal(t, err).Lines, "the gh call's standard input: "+leakguardtest.PairClass)
	})
	t.Run("standard input nothing names is read decoded", func(t *testing.T) {
		// The name with its l written as a JSON escape, so that the raw read does not match.
		escaped := `{"body":"the ` + strings.Replace(leakguardtest.PairName, "l", `\u006c`, 1) + ` and the ` + leakguardtest.PairWord + `"}`
		err := ghFieldCall(t, GhRequest{Args: []string{"api", endpoint, "--input", "-"}, Stdin: strings.NewReader(escaped), Sent: &GhSent{}})
		reports(t, refusal(t, err).Lines, "the gh call's standard input, JSON .body: "+leakguardtest.PairClass)
	})
}

// GitHub stores a release asset uploaded to uploads.github.com as the bytes gh sends, so a `gh
// api` body sent to a whole URL on any host but api.github.com is read whole, and not only one
// JSON string at a time as a body GitHub's REST API parses is: an asset that is one compact JSON
// object whose two fields hold the fixture's two words is refused, as an --input file, as
// --input - and as standard input the caller names the request body, the report naming the
// whole body. Beside the whole body, the strings of a body that is one JSON document are read
// decoded, as whoever parses the asset reads them: the fixture's one word with a letter behind a
// `\u` escape is refused, as an --input file and as --input -, the report naming the string by
// its JSON path.
// The host is api.github.com in ASCII case alone: in upper case the upload's host is another host
// still, and a host holding a rune above ASCII is another host even where Unicode lowering makes
// api.github.com of it. The same asset sent to the REST API's whole URL passes: the host decides.
// Fields with no --input are the body gh sends, one JSON document the guard would read one field
// at a time, so such a call is a setup failure that names the endpoint and --input; so is one
// whose last --input names nothing, since gh keeps the last --input as pflag keeps any option's
// value. Beside --input gh puts the fields in the URL's query string, which GitHub stores apart
// as the asset's name and label, and they are read apart. A GET puts them in the query string
// too, under -X GET and under --method=get alike, since gh reads the method in any case, and they
// are read apart.
func TestAnUploadsBodyIsReadAsGitHubStoresIt(t *testing.T) {
	const upload = "https://uploads.github.com/repos/invented-org/invented-repo/releases/1/assets?name=a.json"
	asset := `{"a":"the ` + leakguardtest.PairName + ` notes","b":"a ` + leakguardtest.PairWord + ` aside"}`
	refusedWhole(t, asset)
	if strings.Contains(asset, "\n") {
		t.Fatalf("the asset %q is on more than one line, so a read of it whole would not join its fields", asset)
	}
	escaped := wordBehindAnEscape(t)
	// fieldsAreTheBody fails the test unless args, a call to the upload, is the setup failure of
	// fields gh sends as the asset.
	fieldsAreTheBody := func(t *testing.T, args []string) {
		t.Helper()
		failed := setupFailure(t, ghFieldCall(t, GhRequest{Args: args}))
		if failed.What != whatGhCall || !strings.Contains(failed.Why, upload) || !strings.Contains(failed.Why, "--input") {
			t.Fatalf("the setup failure is %q, want one of %s whose why names %s and --input", failed.Error(), whatGhCall, upload)
		}
	}
	t.Run("--input <file> is refused", func(t *testing.T) {
		body := fieldFile(t, "a.json", asset)
		err := ghFieldCall(t, GhRequest{Args: []string{"api", upload, "--input", body}})
		reports(t, refusal(t, err).Lines, body+": "+leakguardtest.PairClass)
	})
	t.Run("--input - is refused", func(t *testing.T) {
		err := ghFieldCall(t, GhRequest{Args: []string{"api", upload, "--input", "-"}, Stdin: strings.NewReader(asset)})
		reports(t, refusal(t, err).Lines, "the gh call's standard input: "+leakguardtest.PairClass)
	})
	t.Run("standard input the caller names --input is refused", func(t *testing.T) {
		err := ghFieldCall(t, GhRequest{
			Args:  []string{"api", upload, "--input", "-"},
			Stdin: strings.NewReader(asset),
			Sent:  stdinParse("--input", "", StdinRequestBody),
		})
		reports(t, refusal(t, err).Lines, "the gh call's standard input: "+leakguardtest.PairClass)
	})
	t.Run("a word behind a JSON escape in --input <file> is refused", func(t *testing.T) {
		body := fieldFile(t, "a.json", escaped)
		err := ghFieldCall(t, GhRequest{Args: []string{"api", upload, "--input", body}})
		reports(t, refusal(t, err).Lines, body+", JSON .a: "+leakguardtest.WordClass)
	})
	t.Run("a word behind a JSON escape in --input - is refused", func(t *testing.T) {
		err := ghFieldCall(t, GhRequest{Args: []string{"api", upload, "--input", "-"}, Stdin: strings.NewReader(escaped)})
		reports(t, refusal(t, err).Lines, "the gh call's standard input, JSON .a: "+leakguardtest.WordClass)
	})
	t.Run("the host in upper case is refused", func(t *testing.T) {
		body := fieldFile(t, "a.json", asset)
		args := []string{"api", "https://UPLOADS.GITHUB.COM/repos/invented-org/invented-repo/releases/1/assets?name=a.json", "--input", body}
		reports(t, refusal(t, ghFieldCall(t, GhRequest{Args: args})).Lines, body+": "+leakguardtest.PairClass)
	})
	t.Run("the REST API's whole URL passes", func(t *testing.T) {
		body := fieldFile(t, "a.json", asset)
		args := []string{"api", "https://api.github.com/repos/invented-org/invented-repo/pulls/1/reviews", "--input", body}
		cleared(t, ghFieldCall(t, GhRequest{Args: args}))
	})
	t.Run("fields and no --input are a setup failure", func(t *testing.T) {
		fieldsAreTheBody(t, []string{"api", upload, "-f", "a=a clean note", "-f", "b=another clean note"})
	})
	t.Run("fields after a last --input that names nothing are a setup failure", func(t *testing.T) {
		body := fieldFile(t, "a.json", `{"a":"a clean note"}`)
		fieldsAreTheBody(t, []string{"api", upload, "--input", body, "--input", "", "-f", "a=a clean note", "-f", "b=another clean note"})
	})
	t.Run("fields beside --input <file> pass", func(t *testing.T) {
		body := fieldFile(t, "a.json", `{"a":"a clean note","b":"another clean note"}`)
		args := []string{"api", upload, "--input", body, "-f", "label=the " + leakguardtest.PairName + " notes", "-f", "name=" + leakguardtest.PairWord + ".json"}
		refusedWhole(t, strings.Join(args, " "))
		cleared(t, ghFieldCall(t, GhRequest{Args: args}))
	})
	for _, method := range [][]string{{"-X", "GET"}, {"--method=get"}} {
		t.Run(strings.Join(method, " ")+" with fields and no --input passes", func(t *testing.T) {
			args := append(append([]string{"api", upload}, method...), "-f", "label=the "+leakguardtest.PairName+" notes", "-f", "name="+leakguardtest.PairWord+".json")
			refusedWhole(t, strings.Join(args, " "))
			cleared(t, ghFieldCall(t, GhRequest{Args: args}))
		})
	}
	t.Run("a host holding a rune above ASCII is refused", func(t *testing.T) {
		body := fieldFile(t, "a.json", asset)
		args := []string{"api", "https://ap\u0130.github.com/repos/invented-org/invented-repo/pulls/1/reviews", "--input", body}
		reports(t, refusal(t, ghFieldCall(t, GhRequest{Args: args})).Lines, body+": "+leakguardtest.PairClass)
	})
}

// GitHub stores a release asset's `name` and `label` query values decoded, as the asset's file
// name and label, and decodes a REST path, where a contents API path is a file's name. So a `gh
// api` endpoint's path and each value of its query are read decoded, beside the endpoint as
// written, each where decoding changes its text: the fixture's one word with a letter
// percent-escaped is refused as an upload's `name=` and as its `label=`, the report naming the
// query value, and as a file's name in a contents API path, the report naming the path. A pair of
// the query that holds `;` or whose escapes do not decode is skipped, as url.ParseQuery skips it,
// and the other pairs are still read: the escaped `name=` beside either is refused all the same.
// A clean query with no escape, a pair whose escapes do not decode, and a whole REST URL whose
// path holds gh's `{owner}` and `{repo}` as written add no record: the endpoint is read as
// written, and the call passes.
func TestAnEndpointsPercentEscapesAreReadDecoded(t *testing.T) {
	const uploads = "https://uploads.github.com/repos/invented-org/invented-repo/releases/1/assets"
	// The fixture's one word with its t percent-escaped, so that no rule reads the word in the
	// endpoint as written.
	escaped := strings.Replace(leakguardtest.Word, "t", "%74", 1)
	clean := func(t *testing.T) string {
		t.Helper()
		return fieldFile(t, "a.json", `{"a":"a clean note"}`)
	}
	t.Run("a percent-escaped name= is refused", func(t *testing.T) {
		args := []string{"api", uploads + "?name=" + escaped + ".json", "--input", clean(t)}
		reports(t, refusal(t, ghFieldCall(t, GhRequest{Args: args})).Lines, "the gh call's argument 1, query name: "+leakguardtest.WordClass)
	})
	t.Run("a percent-escaped label= is refused", func(t *testing.T) {
		args := []string{"api", uploads + "?name=a.json&label=the%20" + escaped + "%20notes", "--input", clean(t)}
		reports(t, refusal(t, ghFieldCall(t, GhRequest{Args: args})).Lines, "the gh call's argument 1, query label: "+leakguardtest.WordClass)
	})
	t.Run("a percent-escaped file name in a contents API path is refused", func(t *testing.T) {
		args := []string{"api", "repos/invented-org/invented-repo/contents/" + escaped + ".md", "-X", "PUT", "-f", "message=a clean commit", "-f", "content=Y2xlYW4="}
		reports(t, refusal(t, ghFieldCall(t, GhRequest{Args: args})).Lines, "the gh call's argument 1, path: "+leakguardtest.WordClass)
	})
	for _, other := range []string{"label=v1;beta", "x=%zz"} {
		t.Run("a percent-escaped name= beside "+other+" is refused", func(t *testing.T) {
			args := []string{"api", uploads + "?name=" + escaped + ".json&" + other, "--input", clean(t)}
			reports(t, refusal(t, ghFieldCall(t, GhRequest{Args: args})).Lines, "the gh call's argument 1, query name: "+leakguardtest.WordClass)
		})
	}
	for _, tc := range []struct{ name, endpoint string }{
		{"a clean query with no escape adds no record", uploads + "?name=a.json&label=a-clean-label"},
		{"a pair whose escapes do not decode adds no record", uploads + "?name=%zz.json"},
		{"a whole REST URL whose path holds gh's placeholders adds no record", "https://api.github.com/repos/{owner}/{repo}/contents/README.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"api", tc.endpoint, "--input", clean(t)}
			want := make([]Record, len(args))
			for i, arg := range args {
				want[i] = Record{Label: argumentLabel(i), Text: arg}
			}
			if got := readGhArgv(args).records; !slices.Equal(got, want) {
				t.Fatalf("readGhArgv(%q) read the records %+v, want the arguments alone, %+v", args, got, want)
			}
			cleared(t, ghFieldCall(t, GhRequest{Args: args}))
		})
	}
}

// `gh workflow run <workflow> --json` reads the run's inputs from standard input as one JSON
// object (cmd/legion/testdata/gh-help/workflow_run.txt), and GitHub keeps each input apart, so a
// bare argv's reader names that input a request body as `legion gh`'s parse does: two inputs
// holding the fixture's two words pass, and gh is handed the input the guard read; one input
// holding both is refused, the report naming it by its JSON path. --json is a boolean whose last
// value counts, so `--json=false` names no input; and beside a field of standard input it is a
// second use, so the use is unknown. Either way an input the guard reads is read raw too, and the
// two inputs on its one line are refused.
//
// Each of these holds for a call whose caller handed its parse (GhRequest.Sent) and for a bare
// argv, under a check that asks for the call's standard input, as ghFieldCall's does, and under
// one that clears the call without asking (Passes), since the guard reads the input a call sends
// whichever check ran (GhRequest.Sent). The shared check an INFRA issue pod installed,
// leak-check-v15, measured on 2026-11-20, clears `gh workflow run <w> --json=true` without asking
// for its standard input: under a guard that read only what the check asked for, the call's one
// input naming the fixture's pair reached GitHub, which answered 404 for the invented workflow.
// Under a check that does not ask, a run whose --json ends false sends no input, and nothing
// reads it.
func TestGhWorkflowRunJSONReadsItsInputsOneFieldAtATime(t *testing.T) {
	apart := `{"name":"` + leakguardtest.PairName + `","greeting":"` + leakguardtest.PairWord + `"}`
	together := `{"name":"the ` + leakguardtest.PairName + ` and the ` + leakguardtest.PairWord + `","greeting":"hello"}`
	refusedWhole(t, apart)
	// parsed is the GhSent `legion gh`'s parse hands for a run (cmd/legion's ghSentFiles):
	// standard input named by each field given `@-`, and as a request body where --json ends
	// true (body).
	parsed := func(body bool, fields ...string) *GhSent {
		sent := &GhSent{}
		for _, field := range fields {
			sent.Names("--field", "-", field, StdinField)
		}
		if body {
			sent.UsesStdin(StdinRequestBody)
		}
		return sent
	}
	// bodies are the runs whose input is a request body: --json ends true, and nothing else
	// names the input. sent is the caller's parse of options.
	bodies := []struct {
		options []string
		sent    *GhSent
	}{
		{[]string{"--json"}, parsed(true)},
		{[]string{"--json=true"}, parsed(true)},
		{[]string{"--json=false", "--json=true"}, parsed(true)},
	}
	// raw are the runs whose input is of no known use, read raw as well as decoded wherever it
	// is read; sends is whether the run sends it at all.
	raw := []struct {
		options []string
		sent    *GhSent
		sends   bool
	}{
		{[]string{"--json=false"}, parsed(false), false},
		{[]string{"--json", "--json=false"}, parsed(false), false},
		{[]string{"--json", "-F", "notes=@-"}, parsed(true, "notes"), true},
		{[]string{"-F", "notes=@-", "--json"}, parsed(true, "notes"), true},
	}
	for _, check := range []struct {
		name, script string
		asks         bool
	}{
		{"a check that asks for standard input", leakguardtest.Reading(0, ""), true},
		{"a check that clears without asking", passes, false},
	} {
		for _, parse := range []bool{true, false} {
			caller := "a bare argv"
			if parse {
				caller = "the caller's parse"
			}
			t.Run(check.name+", "+caller, func(t *testing.T) {
				// call is GhCall's answer to the run of options whose standard input is stdin,
				// handed sent where the caller parsed it.
				call := func(t *testing.T, sent *GhSent, stdin string, options ...string) (GhAnswer, error) {
					t.Helper()
					req := GhRequest{
						Args:  append([]string{"workflow", "run", "triage.yml", "-R", "acme/widgets"}, options...),
						Stdin: strings.NewReader(stdin),
						Repo:  "invented-org/invented-repo",
					}
					if parse {
						req.Sent = sent
					}
					return guardWith(t, leakguardtest.Write(t, t.TempDir(), check.script), Public).GhCall(bound(t), req)
				}
				for _, tc := range bodies {
					spelled := strings.Join(tc.options, " ")
					t.Run("two inputs of "+spelled+" pass", func(t *testing.T) {
						answer, err := call(t, tc.sent, apart, tc.options...)
						cleared(t, err)
						if string(answer.Stdin) != apart {
							t.Errorf("gh is handed %q, want the input the guard read, %q", answer.Stdin, apart)
						}
					})
					t.Run("one input of "+spelled+" holding both is refused", func(t *testing.T) {
						_, err := call(t, tc.sent, together, tc.options...)
						reports(t, refusal(t, err).Lines, "the gh call's standard input, JSON .name: "+leakguardtest.PairClass)
					})
				}
				for _, tc := range raw {
					spelled := strings.Join(tc.options, " ")
					if !tc.sends && !check.asks {
						t.Run("the input of "+spelled+", which it does not send, is not read", func(t *testing.T) {
							answer, err := call(t, tc.sent, apart, tc.options...)
							cleared(t, err)
							if answer.Stdin != nil {
								t.Errorf("the guard read %q, an input the call does not send and the check did not ask for", answer.Stdin)
							}
						})
						continue
					}
					t.Run("the input of "+spelled+" is read raw", func(t *testing.T) {
						_, err := call(t, tc.sent, apart, tc.options...)
						reports(t, refusal(t, err).Lines, "the gh call's standard input: "+leakguardtest.PairClass)
					})
				}
			})
		}
	}
}

// A JSON body nested thousands of objects deep is read one string at a time under a label of
// bounded length: a chain of 9,999 objects whose innermost value names the fixture's pair is
// refused, the report naming the value by the path's first two steps, "…" and its last five. The
// records the document is read as hold under 2 MB of labels altogether, one for each of its 9,999
// names and one for the value, where labels holding each whole path would hold about 100 MB.
func TestGhJSONReadsOneFieldOfADeepDocumentUnderABoundedLabel(t *testing.T) {
	const depth = 9999
	document := strings.Repeat(`{"a":`, depth) + `"` + leakguardtest.PairName + " " + leakguardtest.PairWord + `"` + strings.Repeat("}", depth)
	const record = "the gh call's standard input"
	label := record + ", JSON .a.a…" + strings.Repeat(".a", 5)
	lines := refusal(t, ghFieldCall(t, GhRequest{Args: []string{"api", "repos/invented-org/invented-repo/issues", "--input", "-"}, Stdin: strings.NewReader(document)})).Lines
	reports(t, lines, label+": "+leakguardtest.PairClass)
	if len(lines) > 0 {
		if shown, _, _ := strings.Cut(lines[0], ": "+leakguardtest.PairClass); len(shown) >= 400 {
			t.Errorf("the report line's label is %d bytes, want under 400", len(shown))
		}
	}
	fields, ok := jsonFields(Record{Label: record, Text: document}, false)
	if !ok {
		t.Fatal("the document was read as no one JSON document")
	}
	if len(fields) != depth+1 {
		t.Errorf("the document is %d records, want %d: one for each name and one for the value", len(fields), depth+1)
	}
	total := 0
	for _, field := range fields {
		total += len(field.Label)
	}
	if total >= 2<<20 {
		t.Errorf("the records' labels total %d bytes, want under 2 MB", total)
	}
}

// A JSON body's own label can be kilobytes long, as the path an argv wrote for `--input <path>`
// is, and every string of the body is labelled under it, so it is cut once before the walk
// (cutLabel): each record's label is shorter than the body's. A refusal shows each line as the
// whole label would show it, for a value, a name and a GraphQL literal that each name the
// fixture's pair, under a label that holds a newline, a `\`, runes of several bytes and a byte
// that is no UTF-8 at each end.
func TestGhJSONShowsALongBodyLabelAsTheWholeLabelWould(t *testing.T) {
	end := "a\nb\\ é漢𝄞 \xff "
	whole := end + strings.Repeat("invented-dir/", 300) + end
	pair := leakguardtest.PairName + " " + leakguardtest.PairWord
	document := `{"body":"` + pair + `","` + pair + `":"a clean value","query":"mutation { createIssue(input: {title: \"` + pair + `\"}) { issue { number } } }"}`
	fields, ok := jsonFields(Record{Label: whole, Text: document}, true)
	if !ok {
		t.Fatal("the document was read as no one JSON document")
	}
	for _, field := range fields {
		if len(field.Label) >= len(whole) {
			t.Errorf("the record of %q is labelled with %d bytes, want fewer than the body's %d", field.Text, len(field.Label), len(whole))
		}
	}
	rules := fixtureRules(t)
	got := refusal(t, rules.Check(bound(t), "the gh call", fields...)).Lines
	want := refusal(t, rules.Check(bound(t), "the gh call",
		Record{Label: whole + ", JSON .body", Text: pair},
		Record{Label: whole + ", JSON " + jqMember(pair) + " (name)", Text: pair},
		Record{Label: whole + ", JSON .query, GraphQL string 1", Text: pair},
	)).Lines
	if len(got) != 3 || !slices.Equal(got, want) {
		t.Errorf("the report is %q, want the three lines of the whole label, %q", got, want)
	}
}

// A `gh api graphql -F query=@<path>` call's query file is labelled with its path as the argv
// wrote it (GhCall), of any length, and each literal of the query is labelled under it, so that
// label is cut once before the literals are labelled (graphQLRecords, cutLabel): a path of 4 KB
// is not repeated in the label of every literal, and each literal's label is shorter than the
// path. A refusal shows each line as the whole path would show it, for a title and a body that
// each name the fixture's pair.
func TestGhJSONGraphQLShowsALongQueryFileLabelAsTheWholeLabelWould(t *testing.T) {
	whole := strings.Repeat("invented-dir/", 300) + "query.graphql"
	pair := leakguardtest.PairName + " " + leakguardtest.PairWord
	query := `mutation { createIssue(input: {title: "` + pair + `", body: "` + pair + `"}) { issue { number } } }`
	literals, ok := graphQLRecords(Record{Label: whole, Text: query})
	if !ok {
		t.Fatal("the query was read as no GraphQL document")
	}
	if len(literals) != 2 {
		t.Fatalf("the query is %d records, want 2: one for each literal", len(literals))
	}
	for k, suffix := range []string{", GraphQL string 1", ", GraphQL string 2"} {
		label := literals[k].Label
		if !strings.HasSuffix(label, suffix) {
			t.Errorf("literal %d is labelled %q, want a label ending %q", k+1, label, suffix)
		}
		if len(label) >= len(whole) {
			t.Errorf("literal %d is labelled with %d bytes, want fewer than the path's %d", k+1, len(label), len(whole))
		}
	}
	rules := fixtureRules(t)
	got := refusal(t, rules.Check(bound(t), "the gh call", literals...)).Lines
	want := refusal(t, rules.Check(bound(t), "the gh call",
		Record{Label: whole + ", GraphQL string 1", Text: pair},
		Record{Label: whole + ", GraphQL string 2", Text: pair},
	)).Lines
	if len(got) != 2 || !slices.Equal(got, want) {
		t.Errorf("the report is %q, want the two lines of the whole label, %q", got, want)
	}
}

// An empty string holds no text a rule can match, and makes no record, as a name or as a value;
// the value of a member whose name is empty is read all the same, at the step `.""`. A name past
// 64 bytes is shown to 64 in the label of each string beneath it, and "…" marks the cut.
func TestGhJSONMakesNoRecordOfAnEmptyStringAndCutsALongName(t *testing.T) {
	long := strings.Repeat("n", 100)
	fields, ok := jsonFields(Record{Label: "the body", Text: `{"title":"","":"a value","list":["",""],"` + long + `":"a long name's value"}`}, false)
	want := []Record{
		{Label: "the body, JSON .title (name)", Text: "title"},
		{Label: `the body, JSON .""`, Text: "a value"},
		{Label: "the body, JSON .list (name)", Text: "list"},
		{Label: "the body, JSON ." + long[:63] + "… (name)", Text: long},
		{Label: "the body, JSON ." + long[:63] + "…", Text: "a long name's value"},
	}
	if !ok || !slices.Equal(fields, want) {
		t.Errorf("jsonFields = %+v, %t; want %+v", fields, ok, want)
	}
}

// The argv is read one argument at a time, and a field's value apart from every other: the
// fixture's two words as the title and the body of one call pass, the joined argv would match,
// and the two in one field are refused, the report naming the field. Every spelling of the four
// field options is read so, a field's name as well as its value, and every other argument, those
// past a bare `--` among them, is a record of its own.
func TestGhJSONArgumentsReadOneFieldAtATime(t *testing.T) {
	const endpoint = "repos/invented-org/invented-repo/issues"
	t.Run("two fields pass", func(t *testing.T) {
		args := []string{"api", endpoint, "-f", "title=" + leakguardtest.PairName, "-f", "body=" + leakguardtest.PairWord}
		refusedWhole(t, strings.Join(args, " "))
		cleared(t, ghFieldCall(t, GhRequest{Args: args}))
	})
	both := "the " + leakguardtest.PairName + " and the " + leakguardtest.PairWord
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"-f", []string{"api", endpoint, "-f", "title=a clean title", "-f", "body=" + both}, "the gh call's field body"},
		{"-f attached", []string{"api", endpoint, "-fbody=" + both}, "the gh call's field body"},
		{"--raw-field", []string{"api", endpoint, "--raw-field", "body=" + both}, "the gh call's field body"},
		{"--raw-field attached", []string{"api", endpoint, "--raw-field=body=" + both}, "the gh call's field body"},
		{"-F", []string{"api", endpoint, "-F", "body=" + both}, "the gh call's field body"},
		{"--field attached", []string{"api", endpoint, "--field=body=" + both}, "the gh call's field body"},
		{"a field's name", []string{"api", "gists", "-f", "files[" + both + ".md][content]=a clean file"}, "the gh call's field files[" + both + ".md][content] (name)"},
		{"an argument", []string{"pr", "create", "--title", both}, "the gh call's argument 3"},
		{"an argument past a double dash", []string{"api", endpoint, "--", "-f", "body=" + both}, "the gh call's argument 4"},
	} {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			reports(t, refusal(t, ghFieldCall(t, GhRequest{Args: tc.args})).Lines, tc.want+": "+leakguardtest.PairClass)
		})
	}
}

// gh has field options in `gh api` and `gh workflow run` alone, and the argv is read so. In every
// other command -f and -F mean other things (`-F` is `--body-file` in `gh pr create`), so an
// argument that looks like a field is one record, read whole: a title written `-fix <name>=<word>`
// is refused by the rule that needs both words, where a reader taking it for the field `ix <name>`
// would read the two words apart. In `gh api` an option's value is that option's value and never
// an option of its own, so a Go template written the same way is read whole too. The inputs of a
// workflow run are fields, read apart: the fixture's two words in one input are refused, the
// report naming the field, and in two inputs pass.
func TestGhFieldsAreReadOnlyWhereGhHasThem(t *testing.T) {
	lookalike := "-fix " + leakguardtest.PairName + "=" + leakguardtest.PairWord
	refusedWhole(t, lookalike)
	both := "the " + leakguardtest.PairName + " and the " + leakguardtest.PairWord
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"gh issue create's title", []string{"issue", "create", "-t", lookalike}, "the gh call's argument 3"},
		{"gh api's template", []string{"api", "repos/invented-org/invented-repo/issues", "-t", lookalike}, "the gh call's argument 3"},
		{"gh workflow run's input", []string{"workflow", "run", "w.yml", "-f", "body=" + both}, "the gh call's field body"},
	} {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			reports(t, refusal(t, ghFieldCall(t, GhRequest{Args: tc.args})).Lines, tc.want+": "+leakguardtest.PairClass)
		})
	}
	t.Run("gh workflow run's two inputs pass", func(t *testing.T) {
		args := []string{"workflow", "run", "w.yml", "-f", "a=" + leakguardtest.PairName, "-f", "b=" + leakguardtest.PairWord}
		refusedWhole(t, strings.Join(args, " "))
		cleared(t, ghFieldCall(t, GhRequest{Args: args}))
	})
}

// graphQLMutation is a one-line createIssue mutation whose title and body literals hold title
// and body as written, escapes and all: its literals are 1 the repository's id, 2 the title and
// 3 the body.
func graphQLMutation(title, body string) string {
	return `mutation { createIssue(input: {repositoryId: "R_invented", title: "` + title + `", body: "` + body + `"}) { issue { number } } }`
}

// graphQLBody is a `gh api graphql --input` request body holding query.
func graphQLBody(t *testing.T, query string) string {
	t.Helper()
	document, err := json.Marshal(map[string]any{"query": query, "variables": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	return string(document)
}

// A GraphQL query is read one string literal at a time, decoded, as GitHub stores each: the
// fixture's two words as a mutation's title and body pass, though the query holds both on its
// one line, and the two in one literal are refused, the report naming the literal. A name written
// with an escape inside a literal is the name GitHub stores; a block string is read decoded; a
// comment holds no literal; and a query that does not lex is read as written. Each of the query's
// three sources is read so — a `query=` field, a `-F query=@<file>` file (`-F=query=@<file>` as
// well, which pflag reads as the same) and a JSON body's `query` — and a REST call's `query`
// field is a field like any other.
func TestGhJSONGraphQLStringsReadOneFieldAtATime(t *testing.T) {
	apart := graphQLMutation("the "+leakguardtest.PairName+" notes", "a "+leakguardtest.PairWord+" aside")
	together := graphQLMutation("a clean title", "the "+leakguardtest.PairName+" and the "+leakguardtest.PairWord)
	refusedWhole(t, apart)
	field := func(query string) GhRequest {
		return GhRequest{Args: []string{"api", "graphql", "-f", "query=" + query}}
	}
	const literal = "the gh call's field query, GraphQL string "
	t.Run("a title and a body pass", func(t *testing.T) {
		cleared(t, ghFieldCall(t, field(apart)))
	})
	t.Run("one literal holding both is refused", func(t *testing.T) {
		reports(t, refusal(t, ghFieldCall(t, field(together))).Lines, literal+"3: "+leakguardtest.PairClass)
	})
	t.Run("a name written with an escape is refused", func(t *testing.T) {
		for _, escape := range []string{`\u006c`, `\u{6C}`} {
			query := graphQLMutation("the "+strings.Replace(leakguardtest.PairName, "l", escape, 1)+" and the "+leakguardtest.PairWord, "a clean body")
			if strings.Contains(query, leakguardtest.PairName) {
				t.Fatalf("the query %q spells the name in its own bytes, so it proves nothing", query)
			}
			reports(t, refusal(t, ghFieldCall(t, field(query))).Lines, literal+"2: "+leakguardtest.PairClass)
		}
	})
	t.Run("a block string is read decoded", func(t *testing.T) {
		query := "mutation { createIssue(input: {repositoryId: \"R_invented\", title: \"a clean title\", body: \"\"\"\n" +
			"      the " + leakguardtest.PairName + " and the " + leakguardtest.PairWord + "\n" +
			"      says \\\"\"\" and goes\n" +
			"    \"\"\"}) { issue { number } } }"
		err := ghFieldCall(t, field(query))
		want := leakguardtest.ReportLine(literal+"3", leakguardtest.PairClass, leakguardtest.PairName+" and the "+leakguardtest.PairWord, 1, "the "+leakguardtest.PairName+" and the "+leakguardtest.PairWord)
		if lines := refusal(t, err).Lines; len(lines) != 1 || lines[0] != want {
			t.Fatalf("the report is %q, want %q", lines, want)
		}
	})
	t.Run("a comment holds no literal", func(t *testing.T) {
		query := "# \"the " + leakguardtest.PairName + " and the " + leakguardtest.PairWord + "\" is a comment\n" + apart
		cleared(t, ghFieldCall(t, field(query)))
	})
	for _, tc := range []struct{ name, query string }{
		{"an unterminated literal", `mutation { createIssue(input: {title: "the ` + leakguardtest.PairName + ` and the ` + leakguardtest.PairWord + `}) }`},
		{"a bad escape", graphQLMutation(`the `+leakguardtest.PairName+` \q and the `+leakguardtest.PairWord, "a clean body")},
		{"an unterminated block string", `mutation { createIssue(input: {body: """the ` + leakguardtest.PairName + ` and the ` + leakguardtest.PairWord + `}) }`},
	} {
		t.Run("a query with "+tc.name+" is read as written", func(t *testing.T) {
			reports(t, refusal(t, ghFieldCall(t, field(tc.query))).Lines, "the gh call's field query: "+leakguardtest.PairClass)
		})
	}
	t.Run("a query file lexes", func(t *testing.T) {
		cleared(t, ghFieldCall(t, GhRequest{Args: []string{"api", "graphql", "-F", "query=@" + fieldFile(t, "query.graphql", apart)}}))
		query := fieldFile(t, "query.graphql", together)
		err := ghFieldCall(t, GhRequest{Args: []string{"api", "graphql", "-F", "query=@" + query}})
		reports(t, refusal(t, err).Lines, query+", GraphQL string 3: "+leakguardtest.PairClass)
	})
	t.Run("a query file named by an attached -F= lexes", func(t *testing.T) {
		cleared(t, ghFieldCall(t, GhRequest{Args: []string{"api", "graphql", "-F=query=@" + fieldFile(t, "query.graphql", apart)}}))
		query := fieldFile(t, "query.graphql", together)
		err := ghFieldCall(t, GhRequest{Args: []string{"api", "graphql", "-F=query=@" + query}})
		reports(t, refusal(t, err).Lines, query+", GraphQL string 3: "+leakguardtest.PairClass)
	})
	t.Run("a query file the caller parsed lexes", func(t *testing.T) {
		query := fieldFile(t, "query.graphql", together)
		err := ghFieldCall(t, GhRequest{
			Args: []string{"api", "graphql", "--field", "query=@" + query},
			Sent: &GhSent{Files: []GhFile{{Option: "--field", Path: query, Field: "query"}}},
		})
		reports(t, refusal(t, err).Lines, query+", GraphQL string 3: "+leakguardtest.PairClass)
	})
	t.Run("a JSON body's query lexes", func(t *testing.T) {
		refusedWhole(t, graphQLBody(t, apart))
		cleared(t, ghFieldCall(t, GhRequest{Args: []string{"api", "graphql", "--input", fieldFile(t, "request.json", graphQLBody(t, apart))}}))
		body := fieldFile(t, "request.json", graphQLBody(t, together))
		err := ghFieldCall(t, GhRequest{Args: []string{"api", "graphql", "--input", body}})
		reports(t, refusal(t, err).Lines, body+", JSON .query, GraphQL string 3: "+leakguardtest.PairClass)
	})
	t.Run("a JSON body's query on standard input lexes", func(t *testing.T) {
		cleared(t, ghFieldCall(t, GhRequest{Args: []string{"api", "graphql", "--input", "-"}, Stdin: strings.NewReader(graphQLBody(t, apart))}))
		err := ghFieldCall(t, GhRequest{Args: []string{"api", "graphql", "--input", "-"}, Stdin: strings.NewReader(graphQLBody(t, together))})
		reports(t, refusal(t, err).Lines, "the gh call's standard input, JSON .query, GraphQL string 3: "+leakguardtest.PairClass)
	})
	t.Run("the endpoint is found past gh api's options", func(t *testing.T) {
		cleared(t, ghFieldCall(t, GhRequest{Args: []string{"api", "-H", "X-Invented: 1", "--paginate", "-iX", "POST", "graphql", "-f", "query=" + apart}}))
	})
	t.Run("a REST call's query field is read as written", func(t *testing.T) {
		err := ghFieldCall(t, GhRequest{Args: []string{"api", "-X", "GET", "search/issues", "-f", "query=" + apart}})
		reports(t, refusal(t, err).Lines, "the gh call's field query: "+leakguardtest.PairClass)
	})
}

// gh reaches GitHub's GraphQL endpoint under more spellings than the bare word `graphql`. Each
// endpoint below answered a GraphQL query from the gh of an INFRA issue pod on 2026-11-19, and
// GitHub parsed a mutation sent through `/graphql` and through `https://api.github.com/graphql`:
// it answered "Could not resolve to a node" for the invented id, so it had decoded the literals.
// gh sends the `query` field as the `query` member of the request body, which GitHub's GraphQL
// parser decodes as it decodes one sent to `graphql`. So a query sent under any of these
// spellings is read one literal at a time, as under `graphql`: the fixture's two words in two
// literals pass, and a name written with an escape inside one literal is refused, from a `query=`
// field and from an `--input` body alike.
func TestGhJSONGraphQLEndpointSpellingsReadOneFieldAtATime(t *testing.T) {
	apart := graphQLMutation("the "+leakguardtest.PairName+" notes", "a "+leakguardtest.PairWord+" aside")
	escaped := graphQLMutation("the "+strings.Replace(leakguardtest.PairName, "l", `\u006c`, 1)+" and the "+leakguardtest.PairWord, "a clean body")
	refusedWhole(t, apart)
	if strings.Contains(escaped, leakguardtest.PairName) {
		t.Fatalf("the query %q spells the name in its own bytes, so it proves nothing", escaped)
	}
	for _, endpoint := range []string{
		"/graphql",
		"graphql?x=1",
		"/graphql?x=1",
		"https://api.github.com/graphql",
		"https://api.github.com/graphql?x=1",
		"https://API.GITHUB.COM/graphql",
	} {
		t.Run(endpoint, func(t *testing.T) {
			t.Run("a query field", func(t *testing.T) {
				cleared(t, ghFieldCall(t, GhRequest{Args: []string{"api", endpoint, "-f", "query=" + apart}}))
				err := ghFieldCall(t, GhRequest{Args: []string{"api", endpoint, "-f", "query=" + escaped}})
				reports(t, refusal(t, err).Lines, "the gh call's field query, GraphQL string 2: "+leakguardtest.PairClass)
			})
			t.Run("an --input body", func(t *testing.T) {
				cleared(t, ghFieldCall(t, GhRequest{Args: []string{"api", endpoint, "--input", fieldFile(t, "request.json", graphQLBody(t, apart))}}))
				body := fieldFile(t, "request.json", graphQLBody(t, escaped))
				err := ghFieldCall(t, GhRequest{Args: []string{"api", endpoint, "--input", body}})
				reports(t, refusal(t, err).Lines, body+", JSON .query, GraphQL string 2: "+leakguardtest.PairClass)
			})
		})
	}
}

// A `gh api graphql` call can take its query from standard input, `-F query=@-`, and gh then sends
// those bytes as the request's GraphQL query: on 2026-11-19 GitHub parsed a mutation sent so from
// the gh of an INFRA issue pod, and answered "Could not resolve to a node" for its invented id.
// So that input is a GraphQL query and is read one literal at a time, as the same query is from a
// `-F query=@<file>` file: the fixture's two words in two literals pass, and a name written with an
// escape inside one literal is refused. Standard input named by `-F <name>=@-` for any other field
// stays text GitHub stores as it stands.
func TestGhJSONGraphQLQueryOnStandardInputReadsOneFieldAtATime(t *testing.T) {
	apart := graphQLMutation("the "+leakguardtest.PairName+" notes", "a "+leakguardtest.PairWord+" aside")
	escaped := graphQLMutation("the "+strings.Replace(leakguardtest.PairName, "l", `\u006c`, 1)+" and the "+leakguardtest.PairWord, "a clean body")
	refusedWhole(t, apart)
	if strings.Contains(escaped, leakguardtest.PairName) {
		t.Fatalf("the query %q spells the name in its own bytes, so it proves nothing", escaped)
	}
	for _, tc := range []struct {
		name string
		sent *GhSent
	}{
		{"read out of the argv", nil},
		{"named by the caller's parse", stdinParse("--field", graphQLQuery, StdinField)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := func(query string) GhRequest {
				return GhRequest{Args: []string{"api", "graphql", "-F", "query=@-"}, Stdin: strings.NewReader(query), Sent: tc.sent}
			}
			cleared(t, ghFieldCall(t, call(apart)))
			reports(t, refusal(t, ghFieldCall(t, call(escaped))).Lines, "the gh call's standard input, GraphQL string 2: "+leakguardtest.PairClass)
		})
	}
}
