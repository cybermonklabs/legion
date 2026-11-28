package textcheck

//ci:reads packages/envoy/**/*.go TestTheStepAndPlanRefusalCodesAreWrittenOnlyWhereContractsGeneratesThem calls refusalCodeCopies on ../.. (refusalCodeRoots,), whose walk reads every .go file under packages/envoy, infra included
//ci:reads packages/daemon/**/*.go TestTheStepAndPlanRefusalCodesAreWrittenOnlyWhereContractsGeneratesThem calls refusalCodeCopies on ../../../daemon (refusalCodeRoots,), whose walk reads every .go file under packages/daemon

import (
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// refusalCodes are the codes of the step write's and the plan write's refusals that no retry
// changes. Each is stated once, in packages/contracts (the STEP_*_CODE and PLAN_*_CODE constants of
// src/dispatch-tools.ts), and generated into internal/contracts: Dispatch's routes answer them from
// there, and the Legion daemon, a Go module of its own, reads them through
// github.com/sjawhar/envoy/steprecord and ends an outbox row on them.
var refusalCodes = []string{
	contracts.StepInputCode,
	contracts.StepWriterCode,
	contracts.StepOtherIssueCode,
	contracts.PlanInputCode,
	contracts.PlanOtherIssueCode,
}

// refusalCodeRoots are the Go modules that answer or read the codes, from this directory: the
// envoy module, with packages/envoy/infra beneath it, and the Legion daemon's.
var refusalCodeRoots = []string{"../..", "../../../daemon"}

// generatedContracts is the one Go file that holds the codes as values, from this directory.
const generatedContracts = "../contracts/generated.go"

// No Go file of those modules holds one of the codes as a value but the generated one: a copy of
// its own drifts from the code Dispatch answers, and the daemon then retries for ever a write that
// Dispatch refuses for good, or a stand-in answers a code no Dispatch sends. Name the generated
// constant instead (contracts.* in the envoy module, steprecord.* in the daemon's).
func TestTheStepAndPlanRefusalCodesAreWrittenOnlyWhereContractsGeneratesThem(t *testing.T) {
	var offences []string
	for _, root := range refusalCodeRoots {
		found, read := refusalCodeCopies(t, root, generatedContracts, refusalCodes)
		if read == 0 {
			t.Fatalf("read no Go file under %s: the check cannot see that module", root)
		}
		offences = append(offences, found...)
	}
	if len(offences) > 0 {
		t.Fatalf("%d Go string literals hold a refusal code that packages/contracts generates:\n\t%s\n"+
			"name the generated constant instead: contracts.* in packages/envoy, steprecord.* in packages/daemon",
			len(offences), strings.Join(offences, "\n\t"))
	}
}

// The check finds a code written as a whole literal, raw or interpreted, and as a JSON string
// inside one, and passes over a comment, a literal that names the code in a sentence, a longer word
// that holds the code, and the generated file. The planted source is built from the generated
// constants, so this file holds no literal of a code of its own.
func TestTheRefusalCodeCheckSeesWhatItGuards(t *testing.T) {
	root := t.TempDir()
	planted := strings.Join([]string{
		"package planted",
		"",
		"// A comment that names " + contracts.PlanInputCode + " is prose.",
		"var whole = " + strconv.Quote(contracts.PlanInputCode),
		"var raw = `" + contracts.StepWriterCode + "`",
		"var answer = `{\"code\":" + strconv.Quote(contracts.PlanOtherIssueCode) + ",\"error\":\"a plan of another issue\"}`",
		"var sentence = \"want 400 " + contracts.StepInputCode + ", the input refused\"",
		"var longer = \"" + contracts.PlanInputCode + "_TOO\"",
		"",
	}, "\n")
	plantedPath := filepath.Join(root, "planted.go")
	if err := os.WriteFile(plantedPath, []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(root, "generated.go")
	if err := os.WriteFile(generated, []byte("package contracts\n\nconst PlanInputCode = "+strconv.Quote(contracts.PlanInputCode)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, read := refusalCodeCopies(t, root, generated, refusalCodes)
	if read != 1 {
		t.Errorf("the check read %d Go files, want 1: the planted one, and not the generated one", read)
	}
	at := filepath.ToSlash(plantedPath)
	want := []string{
		at + ":4: " + contracts.PlanInputCode,
		at + ":5: " + contracts.StepWriterCode,
		at + ":6: " + contracts.PlanOtherIssueCode,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the check found\n\t%s\nwant\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
}

// refusalCodeCopies names, as file:line: code, every Go string literal under root that holds one
// of codes as a value: the literal's whole text, or a JSON string ("CODE") inside it, and counts
// the Go files it read. The file generated names is passed over, and so are node_modules, vendor
// and hidden directories. Comments are not literals, so prose about a code is not read, and neither
// is a literal that names a code in a sentence, as a failure message does.
func refusalCodeCopies(t *testing.T, root, generated string, codes []string) (offences []string, read int) {
	t.Helper()
	skip, err := filepath.Abs(generated)
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); name == "node_modules" || name == "vendor" || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if absolute, err := filepath.Abs(path); err != nil {
			return err
		} else if absolute == skip {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		read++
		files := token.NewFileSet()
		file := files.AddFile(path, files.Base(), len(source))
		var lex scanner.Scanner
		lex.Init(file, source, nil, 0)
		for {
			position, kind, literal := lex.Scan()
			if kind == token.EOF {
				break
			}
			if kind != token.STRING {
				continue
			}
			text, err := strconv.Unquote(literal)
			if err != nil {
				text = literal
			}
			for _, code := range codes {
				if text == code || strings.Contains(text, strconv.Quote(code)) {
					offences = append(offences, filepath.ToSlash(path)+":"+strconv.Itoa(files.Position(position).Line)+": "+code)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the Go files under %s: %v", root, err)
	}
	slices.Sort(offences)
	return offences, read
}
