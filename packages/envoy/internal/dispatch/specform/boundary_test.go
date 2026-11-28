package specform

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// quotedName is the first quoted string of a finding's detail: the form that the rule read.
var quotedName = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// referenceNames is "<rule> <form>" for each finding of the reference rules on body under c, in
// order. The text is a name, so no rule of a sentence reads it.
func referenceNames(t *testing.T, c Context, body string) []string {
	t.Helper()
	var names []string
	for _, finding := range check(c, Input{Texts: []Text{{Node: "n", Kind: KindName, Body: body, Where: where}}}) {
		name, err := strconv.Unquote(quotedName.FindString(finding.Detail))
		if err != nil {
			t.Fatalf("the detail %q quotes no form: %v", finding.Detail, err)
		}
		names = append(names, string(finding.Rule)+" "+name)
	}
	return names
}

// Each form of a reference rule is a whole word: a character of a word (a letter, a mark, a decimal
// digit or connector punctuation such as "_", of any script) right before it or right after it
// joins it to a longer word, which names nothing, and so do the characters that continue the form
// (wordForm). For each form, the form alone, joined to a word on its left and joined to a word on
// its right, with an ASCII letter and with a letter of another script, and a letter of another
// script inside it. Where a letter is part of the form itself (an email address, a handle, the key
// of an incident), the joined text is a longer form of the same kind. A form whose alphabet is ASCII
// by the rule of its system (a key of Dispatch, an id, the owner and the repository of GitHub, a
// number) holds no letter of another script, so such a letter inside it makes it no form. A
// format character (\p{Cf}) inside or beside a form, such as a zero-width space or a soft hyphen,
// hides no form, as the page shows none: the form stands where the words name it with each format
// character taken out or read as a space, and the finding quotes the form with it.
func TestEachReferenceFormIsAWholeWord(t *testing.T) {
	const id = "0195f3c2-7b1d-7c3e-9a4f-2b6d8e1f0a37"
	for _, test := range []struct {
		form, body string
		want       []string
	}{
		{"a key", "ACME-12", []string{"dispatch-link ACME-12"}},
		{"a key", "xACME-12", nil},
		{"a key", "ACME-12x", nil},
		{"a key", "ACME-12.", []string{"dispatch-link ACME-12"}},
		{"a key with a zero-width space", "ACME-\u200b12", []string{"dispatch-link ACME-\u200b12"}},
		{"a key with a soft hyphen", "AC\u00adME-12", []string{"dispatch-link AC\u00adME-12"}},
		{"the control: a key after a letter and a zero-width space, read as a space", "x\u200bACME-12", []string{"dispatch-link ACME-12"}},
		{"a link with a zero-width space", "dispatch://ACME-\u200b12", []string{"dispatch-link dispatch://ACME-\u200b12"}},
		{"a number of a pull request with a word joiner", "#1\u20602", []string{"pull-request #1\u20602"}},
		{"INC-<n> with a soft hyphen", "INC-\u00ad12", []string{"incident INC-\u00ad12"}},
		{"an email address with a soft hyphen", "ada\u00ad@example.com", []string{"person ada\u00ad@example.com"}},
		{"an id", id, []string{"dispatch-link " + id}},
		{"an id", "x" + id, nil},
		{"an id", id + "x", nil},
		{"incident number", "incident number 12", []string{"incident incident number 12"}},
		{"incident number", "xincident number 12", nil},
		{"incident number", "incident number 12x", nil},
		{"incident #", "incident#12", []string{"incident incident#12"}},
		{"incident #", "incident #12x", nil},
		{"an incident's date", "incident 2026-11-17", []string{"incident incident 2026-11-17"}},
		{"an incident's date", "incident 2026-11-17x", nil},
		{"an incident's key", "incident sev-12", []string{"incident incident sev-12"}},
		{"an incident's key", "incident sev-12x", nil},
		{"INC-<n>", "INC-12", []string{"incident INC-12"}},
		{"INC-<n>", "xINC-12", nil},
		{"INC-<n>", "INC-12x", nil},
		{"pull request number", "PR number 12", []string{"pull-request PR number 12"}},
		{"pull request number", "xPR number 12", nil},
		{"pull request number", "PR number 12x", nil},
		{"pull request #", "PR#12", []string{"pull-request PR#12"}},
		{"pull request #", "pull request #12", []string{"pull-request pull request #12"}},
		{"pull request #", "PR #12x", nil},
		{"<owner>/<repo>#<n>", "acme/widgets#12", []string{"pull-request acme/widgets#12"}},
		{"<owner>/<repo>#<n>", "docs/acme/widgets#12", nil},
		{"<owner>/<repo>#<n>", "acme/widgets#12x", nil},
		{"<owner>/<repo>#<n>, an owner starts with a letter or a digit", ".acme/widgets#12", nil},
		{"<owner>/<repo>#<n>, an owner starts with a letter or a digit", "-acme/widgets#12", nil},
		{"<owner>/<repo>#<n>, an owner starts with a letter or a digit", "_acme/widgets#12", nil},
		{"<owner>/<repo>#<n>, an owner that starts with a digit", "0acme/widgets#12", []string{"pull-request 0acme/widgets#12"}},
		{"#<n>", "#12", []string{"pull-request #12"}},
		{"#<n>", "(#12).", []string{"pull-request #12"}},
		{"#<n>", "x#12", nil},
		{"#<n>", "#12x", nil},
		{"#<n>, a colour of six digits and letters", "#12abcd", nil},
		{"#<n>, a colour of six digits and letters", "#123abc", nil},
		{"#<n>, the stated limit: a colour of three digits", "#123", []string{"pull-request #123"}},
		{"the address of a pull request", "github.com/o/r/pull/12", []string{"pull-request github.com/o/r/pull/12"}},
		{"the address of a pull request", "xgithub.com/o/r/pull/12", nil},
		{"the address of a pull request", "github.com/o/r/pull/12draft", nil},
		{"the address of a pull request", "github.com/o/r/pull/12/files", []string{"pull-request github.com/o/r/pull/12"}},
		{"an email address", "ada@example.com", []string{"person ada@example.com"}},
		{"an email address", "xada@example.com", []string{"person xada@example.com"}},
		{"an email address", "ada@example.comx", []string{"person ada@example.comx"}},
		{"an email address", "ada@example.com5", nil},
		{"a handle", "@legion", []string{"person @legion"}},
		{"a handle", "x@legion", nil},
		{"a handle", "@legion/core", nil},
		{"a handle, an npm scope", "@types/node", nil},
		{"a key, ASCII by Dispatch's rule", "ACMÉ-12", nil},
		{"a key, ASCII by Dispatch's rule", "éACME-12", nil},
		{"a key, ASCII by Dispatch's rule", "ACME-12é", nil},
		{"an id, hexadecimal", "0195f3c2-7b1d-7c3e-9a4f-2b6d8e1f0é37", nil},
		{"an id, hexadecimal", "é" + id, nil},
		{"an id, hexadecimal", id + "é", nil},
		{"an incident's key", "incident sév-12", []string{"incident incident sév-12"}},
		{"an incident's key", "éincident sev-12", nil},
		{"an incident's key", "incident sev-12é", nil},
		{"incident number", "incident number 12é", nil},
		{"INC-<n>", "éINC-12", nil},
		{"INC-<n>", "INC-12é", nil},
		{"pull request number", "éPR number 12", nil},
		{"pull request #", "PR #12é", nil},
		{"<owner>/<repo>#<n>, ASCII by GitHub's rule", "acmé/widgets#12", nil},
		{"<owner>/<repo>#<n>, ASCII by GitHub's rule", "éacme/widgets#12", nil},
		{"<owner>/<repo>#<n>, ASCII by GitHub's rule", "acme/widgets#12é", nil},
		{"#<n>", "é#12", nil},
		{"#<n>", "#12é", nil},
		{"#<n>, the stated limit: a script with no space between words", "见#12", nil},
		{"#<n>, the stated limit: a colour of digits only", "#000000", []string{"pull-request #000000"}},
		{"the address of a pull request, ASCII by GitHub's rule", "github.com/ö/r/pull/12", nil},
		{"the address of a pull request", "égithub.com/o/r/pull/12", nil},
		{"the address of a pull request", "github.com/o/r/pull/12é", nil},
		{"the address of a pull request, the stated limit: a script with no space between words",
			"参见github.com/acme/widgets/pull/12", nil},
		{"an email address", "josé.garcia@example.com", []string{"person josé.garcia@example.com"}},
		{"an email address", "ada@exämple.com", []string{"person ada@exämple.com"}},
		{"an email address", "éada@example.com", []string{"person éada@example.com"}},
		{"an email address", "ada@example.comé", []string{"person ada@example.comé"}},
		{"an email address, a letter with a combining mark", "jose\u0301@example.com", []string{"person jose\u0301@example.com"}},
		{"an email address, then a colon and a space", "ada@example.com: she", []string{"person ada@example.com"}},
		{"an email address, then a colon at the end", "ada@example.com:", []string{"person ada@example.com"}},
		{"an SSH address: a colon and a character of a path after it", "git@github.com:owner/repo.git", nil},
		{"an SSH address whose path starts at the root", "deploy@build.example.com:/srv/app", nil},
		{"an SSH address whose path starts at the home folder", "git@github.com:~/repo.git", nil},
		{"the URL form of an SSH address: a / before it", "ssh://git@github.com/acme/widgets.git", nil},
		{"the URL form of an SSH address: a / before it", "git+ssh://git@github.com/acme/widgets.git", nil},
		{"the cost of the rule: a / before an address", "x/ada@example.com", nil},
		{"an email address in brackets, then a colon", "(ada@example.com:)", []string{"person ada@example.com"}},
		{"an email address, then a colon and a comma", "ada@example.com:,", []string{"person ada@example.com"}},
		{"an email address, then a colon and white space of another kind", "ada@example.com:\tshe", []string{"person ada@example.com"}},
		{"an email address, then a colon and a no-break space", "ada@example.com:\u00a0she", []string{"person ada@example.com"}},
		{"an email address, then a colon and a semicolon", "ada@example.com:;", []string{"person ada@example.com"}},
		{"an email address, then a colon and a stop of a question", "ada@example.com:?", []string{"person ada@example.com"}},
		{"an email address, then a colon and a stop of an exclamation", "ada@example.com:!", []string{"person ada@example.com"}},
		{"an email address in square brackets, then a colon", "[ada@example.com:]", []string{"person ada@example.com"}},
		{"an email address in quotes, then a colon", "\u201cada@example.com:\u201d", []string{"person ada@example.com"}},
		{"an scp address before a path in double quotes", `deploy@build.example.com:"/srv/my reports/"`, nil},
		{"an scp address before a path in single quotes", "deploy@build.example.com:'/srv/my reports/'", nil},
		{"an rsync address before a path from a variable", "deploy@build.example.com:$HOME/app/", nil},
		{"an scp address before a pattern", "deploy@build.example.com:*.log", nil},
		{"an rsync address before a module", "ops@backup.example.com::backups", nil},
		{"an email address, then a colon and a straight quote at the end", `ada@example.com:"`, []string{"person ada@example.com"}},
		{"an email address in straight double quotes, then a colon", `The field reads "ada@example.com:" each day.`, []string{"person ada@example.com"}},
		{"an email address in straight single quotes, then a colon", "The field reads 'ada@example.com:' each day.", []string{"person ada@example.com"}},
		{"an email address in straight quotes, then a colon and a comma", `"ada@example.com:", she says`, []string{"person ada@example.com"}},
		{"the stated limit: an email address in straight quotes, then a colon, a quote and a full stop", `The field reads "ada@example.com:".`, nil},
		{"an scp address before a path in quotes, in words", `Legion copies it to deploy@build.example.com:"/srv/my reports/" each day.`, nil},
		{"the control: an email address after mailto:", "mailto:ada@example.com", []string{"person ada@example.com"}},
		{"a handle", "@josé", []string{"person @josé"}},
		{"a handle", "é@legion", nil},
		{"a handle, a letter with a combining mark", "@jose\u0301", []string{"person @jose\u0301"}},
		{"a handle, the stated limit: a word that starts with @", "@media", []string{"person @media"}},
	} {
		if got := referenceNames(t, planted(), test.body); !slices.Equal(got, test.want) {
			t.Errorf("%s: %q gives %q, want %q", test.form, test.body, got, test.want)
		}
	}
}

// Each part of a person's name is a whole word of wordClass, as each form of a name is: a character
// of a word right before it or right after it makes it part of a longer word, which is no part of
// the name. A part of a person's email that holds a digit or a mark is one word too, and so is a
// word of an approved component name. A format character (\p{Cf}), such as a soft hyphen, a
// zero-width space or a word joiner, is no character that the page shows, so it hides no name: a
// name stands where the words name it with each format character taken out, which joins the
// characters on its two sides, or read as a space, which parts them, and the finding quotes the
// name with its format characters. The stated limit: Dispatch compares no normal forms, so a
// letter written with a combining mark in the text names no part that the email writes as one
// composed letter.
func TestEachPartOfAPersonsNameIsAWholeWord(t *testing.T) {
	const ada = "ada.lovelace@example.com"
	for _, test := range []struct {
		name, person, system, body string
		want                       []string
	}{
		{"the control: the name", ada, "", "Dispatch asks Ada Lovelace.", []string{"person Ada Lovelace"}},
		{"the control: a comma and a digit apart", ada, "", "Dispatch asks Ada Lovelace, 2 times.", []string{"person Ada Lovelace"}},
		{"a digit after a part", ada, "", "Dispatch asks Ada Lovelace2.", nil},
		{"a digit before a part", ada, "", "Dispatch asks 2Ada Lovelace.", nil},
		{"_ after a part", ada, "", "Dispatch asks Ada Lovelace_x.", nil},
		{"_ before a part", ada, "", "Dispatch asks x_Ada Lovelace.", nil},
		{"a combining mark after a part", ada, "", "Dispatch asks Ada Lovelace\u0301.", nil},
		{"a part of the email with a digit", "ada2.lovelace@example.com", "", "Dispatch asks Ada2 Lovelace.", []string{"person Ada2 Lovelace"}},
		{"a combining mark in the email and in the text", "jose\u0301.garcia@example.com", "", "Dispatch asks Jose\u0301 Garcia.",
			[]string{"person Jose\u0301 Garcia"}},
		{"a composed letter in the email and in the text", "josé.garcia@example.com", "", "Dispatch asks José Garcia.", []string{"person José Garcia"}},
		{"the stated limit: a composed letter in the email, a combining mark in the text", "josé.garcia@example.com", "",
			"Dispatch asks Jose\u0301 Garcia.", nil},
		{"a component name that _ joins is one word", "dispatch.admin@example.com", "Dispatch_Admin", "Dispatch asks Dispatch Admin.",
			[]string{"person Dispatch Admin"}},
		{"the control: a component name of two words", "dispatch.admin@example.com", "Dispatch Admin", "Dispatch asks Dispatch Admin.", nil},
		{"a soft hyphen inside a part", ada, "", "Dispatch asks Ada Love\u00adlace.", []string{"person Ada Love\u00adlace"}},
		{"a zero-width space before the space", ada, "", "Dispatch asks Ada\u200b Lovelace.", []string{"person Ada\u200b Lovelace"}},
		{"a word joiner between the parts", ada, "", "Dispatch asks Ada\u2060Lovelace.", []string{"person Ada\u2060Lovelace"}},
		{"a zero-width space between the parts", ada, "", "Dispatch asks Ada\u200bLovelace.", []string{"person Ada\u200bLovelace"}},
		{"a soft hyphen in each part, and a zero-width space after the name", ada, "", "Dispatch asks A\u00adda Love\u00adlace\u200b.",
			[]string{"person A\u00adda Love\u00adlace"}},
		{"the control: a letter between the parts", ada, "", "Dispatch asks AdaxLovelace.", nil},
		{"the control: no space between the parts", ada, "", "Dispatch asks AdaLovelace.", nil},
		{"the control: a real space between the parts", ada, "", "Dispatch asks Ada Lovelace.", []string{"person Ada Lovelace"}},
		{"the control: a soft hyphen between a letter and a part, read as a space", ada, "", "Dispatch asks x\u00adAda Lovelace.",
			[]string{"person Ada Lovelace"}},
		{"the control: a word joiner between a part and a digit, read as a space", ada, "", "Dispatch asks Ada Lovelace\u20602.",
			[]string{"person Ada Lovelace"}},
	} {
		context := planted(test.person)
		if test.system != "" {
			context.SystemNames = []string{test.system}
		}
		if got := referenceNames(t, context, test.body); !slices.Equal(got, test.want) {
			t.Errorf("%s: %q with %s gives %q, want %q", test.name, test.body, test.person, got, test.want)
		}
	}
}

// No form can read a word otherwise than wordClass does: newWordForm refuses a form, a left or a
// right that holds a notion of a word of the regular expressions, each of which reads ASCII only,
// and so does wordRegexp, which also builds the words of a person's name (nameWord). The control:
// the same form with wordClass is taken.
func TestNoFormHoldsAnotherNotionOfAWord(t *testing.T) {
	refuses := func(form, left, right string) (refused bool) {
		defer func() { refused = recover() != nil }()
		newWordForm(form, left, right)
		return false
	}
	for _, test := range []struct{ form, left, right string }{
		{`[\w.+-]+@example\.com`, "", ""},
		{`\bACME-[0-9]+`, "", ""},
		{`#[0-9]+\B`, "", ""},
		{`@[^\W]+`, "", ""},
		{`@[[:word:]]+`, "", ""},
		{`#[0-9]+`, `\w`, ""},
		{`#[0-9]+`, "", `\w`},
	} {
		if !refuses(test.form, test.left, test.right) {
			t.Errorf("newWordForm(%q, %q, %q) is taken, want a refusal", test.form, test.left, test.right)
		}
	}
	for _, expr := range []string{`\w+`, `[[:word:]]+`} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("wordRegexp(%q) is taken, want a refusal", expr)
				}
			}()
			wordRegexp(expr)
		}()
	}
	if refuses(`[`+wordClass+`.+\-]+@example\.com`, "", "") {
		t.Error("the control: a form built on wordClass is refused")
	}
}

// patternFunctions is each function of package regexp that takes a pattern. QuoteMeta takes text
// and makes none, so it is not one.
var patternFunctions = []string{"Compile", "CompilePOSIX", "MustCompile", "MustCompilePOSIX", "Match", "MatchReader", "MatchString"}

// patternsOutsideTheGuard reads source, the file path of the package, and counts the calls of a
// function of package regexp that takes a pattern (patternFunctions) inside wordRegexp, and names
// each one outside it. A dot import of regexp is named too, since a call through it names no
// package, and so is each other name that the file gives the package.
func patternsOutsideTheGuard(t *testing.T, path, source string) (inside int, outside []string) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	name := ""
	for _, spec := range file.Imports {
		if spec.Path.Value != `"regexp"` {
			continue
		}
		name = "regexp"
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "." {
			return 0, []string{path + `: import . "regexp"`}
		}
	}
	if name == "" {
		return 0, nil
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		guard := ok && function.Recv == nil && function.Name.Name == "wordRegexp"
		ast.Inspect(declaration, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := selector.X.(*ast.Ident); !ok || pkg.Name != name || !slices.Contains(patternFunctions, selector.Sel.Name) {
				return true
			}
			if guard {
				inside++
			} else {
				outside = append(outside, path+": "+name+"."+selector.Sel.Name)
			}
			return true
		})
	}
	return inside, outside
}

// Every regular expression of the package is built through the guard: no call of a function of
// package regexp that takes a pattern stands outside wordRegexp in a file of the package that is no
// test, and no file imports regexp into its own names, so no form of a name can hold a notion of a
// word that the guard did not read. The test sees the one call inside wordRegexp, so it reads the
// files it names, and it names each planted file of its controls: a call of MustCompile, of Compile
// through another name of the package, of MatchString and of Match, and a dot import. The control
// in the other direction: a call of QuoteMeta, which takes no pattern, is no finding.
func TestEveryPatternOfTheRulesIsBuiltThroughTheGuard(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	read, inside, outside := 0, 0, []string{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		read++
		in, out := patternsOutsideTheGuard(t, path, string(source))
		inside, outside = inside+in, append(outside, out...)
	}
	if read < 2 || inside == 0 {
		t.Fatalf("the test read %d files of the package and saw %d calls in wordRegexp, so it reads nothing", read, inside)
	}
	if len(outside) > 0 {
		t.Errorf("the package builds %q outside wordRegexp; build each pattern through wordRegexp", outside)
	}
	const head = "package specform\n\n"
	for _, plant := range []struct{ name, source string }{
		{"a call of MustCompile", head + "import \"regexp\"\n\nvar x = regexp.MustCompile(`\\bx`)\n"},
		{"a call of Compile through another name", head + "import re \"regexp\"\n\nvar x, _ = re.Compile(`\\bx`)\n"},
		{"a call of MatchString", head + "import \"regexp\"\n\nvar x, _ = regexp.MatchString(`\\bx`, \"x\")\n"},
		{"a call of Match", head + "import \"regexp\"\n\nvar x, _ = regexp.Match(`\\bx`, nil)\n"},
		{"a dot import", head + "import . \"regexp\"\n\nvar x = MustCompile(`\\bx`)\n"},
	} {
		if _, out := patternsOutsideTheGuard(t, "plant.go", plant.source); len(out) != 1 {
			t.Errorf("the control %s: the guard names %q, want the one planted call", plant.name, out)
		}
	}
	if _, out := patternsOutsideTheGuard(t, "plant.go", head+"import \"regexp\"\n\nvar x = regexp.QuoteMeta(\"x.y\")\n"); len(out) != 0 {
		t.Errorf("the control a call of QuoteMeta: the guard names %q, want nothing", out)
	}
}

// A bare key or id names a record of Dispatch and links to none: the finding says so. A reference
// links to the record.
func TestABareKeyOrIdNamesARecordAndLinksToNone(t *testing.T) {
	const id = "12345678-1234-1234-1234-123456789abc"
	for _, test := range []struct{ body, detail string }{
		{"Dispatch stores the value `" + id + "`.", `it names a record of Dispatch by its id: "` + id + `"`},
		{"Dispatch keeps ACME-3 for each reader.", `it names a record of Dispatch by its key: "ACME-3"`},
		{"Dispatch keeps dispatch://ACME-4 for each reader.", `it links to a record of Dispatch: "dispatch://ACME-4"`},
	} {
		text := statement(test.body)
		if got, want := check(planted(), Input{Texts: []Text{text}}), []Finding{finding(text, "dispatch-link", test.detail)}; !sameFindings(got, want) {
			t.Errorf("Check(%q) = %#v, want %#v", test.body, got, want)
		}
	}
}
