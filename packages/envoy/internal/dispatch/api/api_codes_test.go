package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

// v1CodeExceptions are the pairs a constant renders under /api/v1 that testdata/v1-codes.txt does
// not hold, each with why.
var v1CodeExceptions = map[string]string{
	"API_V1_ENDED 410":   "R3.4's end code, new",
	"ORIGIN_REFUSED 403": "R3.2's code for the cookie-origin guard, new",
	"HOST_MISMATCH 421":  "a routes code of c8a44f97, which answers every path in dev sign-in mode, unchanged",
}

// v1Pair is the line of testdata/v1-codes.txt that names what code renders under /api/v1.
func v1Pair(code apicode.Code) string {
	if code.StatusFromCall() {
		return code.V1Name() + " *"
	}
	return code.V1Name() + " " + strconv.Itoa(code.V1Status())
}

// readV1Codes is every pair testdata/v1-codes.txt lists: a `#` line is a comment, and a pair's
// line may name the merge that added it after " #".
func readV1Codes(t *testing.T) map[string]bool {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "v1-codes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	listed := map[string]bool{}
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pair, _, _ := strings.Cut(line, " #")
		fields := strings.Fields(pair)
		if len(fields) != 2 {
			t.Fatalf("v1-codes.txt line %q is not CODE STATUS", line)
		}
		listed[fields[0]+" "+fields[1]] = true
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	return listed
}

// v1CodeMismatches names every pair a /api/v1 constant of codes renders that listed does not hold
// (but the named exceptions), every listed pair no constant renders, and every exception that is
// no constant's pair or that the list holds after all.
func v1CodeMismatches(codes []apicode.Code, listed map[string]bool) []string {
	var mismatches []string
	rendered := map[string]bool{}
	for _, code := range codes {
		if !code.InV1() {
			continue
		}
		pair := v1Pair(code)
		rendered[pair] = true
		if _, excepted := v1CodeExceptions[pair]; !excepted && !listed[pair] {
			mismatches = append(mismatches, fmt.Sprintf("%s (%s) renders %q under /api/v1, which c8a44f97 never answered", code, code.Name(), pair))
		}
	}
	for pair := range listed {
		if !rendered[pair] {
			mismatches = append(mismatches, fmt.Sprintf("v1-codes.txt lists %q, which no constant renders under /api/v1", pair))
		}
	}
	for pair, why := range v1CodeExceptions {
		if !rendered[pair] || listed[pair] {
			mismatches = append(mismatches, fmt.Sprintf("the exception %q (%s) is no constant's v1 pair, or the list holds it", pair, why))
		}
	}
	slices.Sort(mismatches)
	return mismatches
}

// /api/v1 answers every code and status it answered at c8a44f97 and no other (R3): every constant
// that renders under /api/v1 renders a pair c8a44f97 wrote, or a later merge on main added, or one
// of the three named exceptions, and every such pair is still some constant's. A v2-only code
// renders nothing under /api/v1, so a new code cannot reach a v1 client.
func TestV1CodesAreTheCodesOfC8a44f97(t *testing.T) {
	listed := readV1Codes(t)
	for _, mismatch := range v1CodeMismatches(apicode.All(), listed) {
		t.Error(mismatch)
	}
	for _, code := range []apicode.Code{apicode.NoRoute, apicode.QueryKeyUnknown, apicode.PositionInvalid, apicode.AskNotFound} {
		if code.InV1() {
			t.Errorf("%s renders under /api/v1, which never answered it", code)
		}
	}

	t.Run("a pair the list lost and a pair no constant renders fail it", func(t *testing.T) {
		short := map[string]bool{"NOT_A_CODE 499": true}
		for pair := range listed {
			if pair != v1Pair(apicode.IssueNotFound) {
				short[pair] = true
			}
		}
		mismatches := v1CodeMismatches(apicode.All(), short)
		lost := fmt.Sprintf("renders %q under /api/v1", v1Pair(apicode.IssueNotFound))
		if len(mismatches) != 2 || !slices.ContainsFunc(mismatches, func(m string) bool { return strings.Contains(m, lost) }) ||
			!slices.Contains(mismatches, `v1-codes.txt lists "NOT_A_CODE 499", which no constant renders under /api/v1`) {
			t.Fatalf("the list without ISSUE_NOT_FOUND 404 and with NOT_A_CODE 499: mismatches %q, want those two", mismatches)
		}
	})
}

// refusalLiteralProblems names each literal in file that makes a refusal outside the writers: an
// apicode.Code or apicode.Condition literal, which only package apicode makes, and an errorBody
// literal anywhere but writeVersionedError, the one writer writeError and writeErrorAt write
// through, and testHookFailure, the test hooks' writer, which keeps its string codes because a
// notification reaches it with no request. writers counts the errorBody literals each allowed
// function holds.
func refusalLiteralProblems(fset *token.FileSet, file *ast.File, writers map[string]int) []string {
	var problems []string
	for _, decl := range file.Decls {
		function := ""
		if fd, ok := decl.(*ast.FuncDecl); ok {
			function = fd.Name.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			literal, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			at := fset.Position(literal.Pos())
			switch kind := literal.Type.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := kind.X.(*ast.Ident); ok && pkg.Name == "apicode" && (kind.Sel.Name == "Code" || kind.Sel.Name == "Condition") {
					problems = append(problems, fmt.Sprintf("%s:%d: an apicode.%s literal outside package apicode", filepath.Base(at.Filename), at.Line, kind.Sel.Name))
				}
			case *ast.Ident:
				if kind.Name != "errorBody" {
					break
				}
				if function == "writeVersionedError" || function == "testHookFailure" {
					writers[function]++
					break
				}
				problems = append(problems, fmt.Sprintf("%s:%d: an errorBody literal outside writeVersionedError", filepath.Base(at.Filename), at.Line))
			}
			return true
		})
	}
	return problems
}

// No source of the api or routes package makes an apicode.Code (whose zero value the writers
// answer 500), and none builds an errorBody but writeVersionedError, the one writer writeError and
// writeErrorAt write through, and testHookFailure, the test hooks' writer. That is all it reads: a
// refusal written with a body of its own through WriteJSON, its code named through refusalPair
// (writeV2DecodeError, writeHandlerError's lost edit, writeClaimError, the issue state's and the
// marks' STATE_STALE), and the routes package's writeCodeError, which takes its code as a string,
// make no literal it reads.
func TestNoSourceMakesACodeOrAnErrorBodyOutsideItsWriters(t *testing.T) {
	writers := map[string]int{}
	for _, dir := range []string{".", filepath.Join("..", "routes")} {
		paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, problem := range refusalLiteralProblems(fset, file, writers) {
				t.Error(problem)
			}
		}
	}
	if writers["writeVersionedError"] != 1 {
		t.Errorf("writeVersionedError holds %d errorBody literals, want the one it writes", writers["writeVersionedError"])
	}

	t.Run("a made-up code and a body of its own fail it", func(t *testing.T) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "scratch.go", `package api
func refuse(w http.ResponseWriter) {
	code := apicode.Code{}
	WriteJSON(w, 400, errorBody{Code: "MADE_UP", Error: code.Name()})
}`, 0)
		if err != nil {
			t.Fatal(err)
		}
		if problems := refusalLiteralProblems(fset, file, map[string]int{}); len(problems) != 2 {
			t.Fatalf("problems %q, want the Code literal and the errorBody literal", problems)
		}
	})
}

// writtenRefusal is what a writer answered: its status and the body's code, with the body's bytes.
type writtenRefusal struct {
	status int
	code   string
	body   string
}

// writeUnder answers a request with write, through withAPIVersion(version) as the mount serves a
// route, or with no version on the request at all for version 0.
func writeUnder(t *testing.T, version apiVersion, write http.HandlerFunc) writtenRefusal {
	t.Helper()
	handler := write
	if version != 0 {
		handler = withAPIVersion(version, write)
	}
	response := httptest.NewRecorder()
	handler(response, httptest.NewRequest(http.MethodPost, "/api/scratch", nil))
	var body errorBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
	return writtenRefusal{status: response.Code, code: body.Code, body: response.Body.String()}
}

// One constant renders its v1 pair on a request the mount marked v1, or left unmarked, and its v2
// pair on a request marked v2, split constants among them; a code the version does not answer is
// the server's mistake and answers 500 INTERNAL rather than a code the version never declared.
func TestWriteErrorAnswersTheRequestsVersion(t *testing.T) {
	for _, test := range []struct {
		what    string
		write   http.HandlerFunc
		v1, v2  writtenRefusal
		v1Exact string
	}{
		{
			what:    "a body past its bound, CAP_EXCEEDED under /api/v1",
			write:   func(w http.ResponseWriter, r *http.Request) { writeError(w, r, apicode.CapExceededTooLarge, "too big") },
			v1:      writtenRefusal{status: http.StatusRequestEntityTooLarge, code: "CAP_EXCEEDED"},
			v2:      writtenRefusal{status: http.StatusRequestEntityTooLarge, code: "REQUEST_TOO_LARGE"},
			v1Exact: `{"code":"CAP_EXCEEDED","error":"too big"}` + "\n",
		},
		{
			what: "a parent loop, returned as an error",
			write: func(w http.ResponseWriter, r *http.Request) {
				(&server{}).writeHandlerError(w, r, errorf(apicode.ParentCycle, "loop"))
			},
			v1: writtenRefusal{status: http.StatusConflict, code: "PARENT_INPUT"},
			v2: writtenRefusal{status: http.StatusConflict, code: "ISSUE_CYCLE"},
		},
		{
			what:  "a code both versions answer alike",
			write: func(w http.ResponseWriter, r *http.Request) { writeError(w, r, apicode.IssueNotFound, "no issue") },
			v1:    writtenRefusal{status: http.StatusNotFound, code: "ISSUE_NOT_FOUND"},
			v2:    writtenRefusal{status: http.StatusNotFound, code: "ISSUE_NOT_FOUND"},
		},
		{
			what:  "a v2-only code",
			write: func(w http.ResponseWriter, r *http.Request) { writeError(w, r, apicode.QueryKeyUnknown, "no such key") },
			v1:    writtenRefusal{status: http.StatusInternalServerError, code: "INTERNAL"},
			v2:    writtenRefusal{status: http.StatusBadRequest, code: "QUERY_KEY_UNKNOWN"},
		},
		{
			what:  "a v1-only code",
			write: func(w http.ResponseWriter, r *http.Request) { writeError(w, r, apicode.APIV1Ended, "ended") },
			v1:    writtenRefusal{status: http.StatusGone, code: "API_V1_ENDED"},
			v2:    writtenRefusal{status: http.StatusInternalServerError, code: "INTERNAL"},
		},
		{
			what:  "the zero Code",
			write: func(w http.ResponseWriter, r *http.Request) { writeError(w, r, apicode.Code{}, "none") },
			v1:    writtenRefusal{status: http.StatusInternalServerError, code: "INTERNAL"},
			v2:    writtenRefusal{status: http.StatusInternalServerError, code: "INTERNAL"},
		},
		{
			what: "the code whose status is the call's",
			write: func(w http.ResponseWriter, r *http.Request) {
				writeErrorAt(w, r, apicode.AnswerNotKept, http.StatusTeapot, "not kept")
			},
			v1: writtenRefusal{status: http.StatusTeapot, code: "ANSWER_NOT_KEPT"},
			v2: writtenRefusal{status: http.StatusTeapot, code: "ANSWER_NOT_KEPT"},
		},
		{
			what:  "that code given no status",
			write: func(w http.ResponseWriter, r *http.Request) { writeError(w, r, apicode.AnswerNotKept, "not kept") },
			v1:    writtenRefusal{status: http.StatusInternalServerError, code: "INTERNAL"},
			v2:    writtenRefusal{status: http.StatusInternalServerError, code: "INTERNAL"},
		},
		{
			what: "another code given a status",
			write: func(w http.ResponseWriter, r *http.Request) {
				writeErrorAt(w, r, apicode.IssueNotFound, http.StatusTeapot, "no issue")
			},
			v1: writtenRefusal{status: http.StatusInternalServerError, code: "INTERNAL"},
			v2: writtenRefusal{status: http.StatusInternalServerError, code: "INTERNAL"},
		},
	} {
		t.Run(test.what, func(t *testing.T) {
			for _, version := range []apiVersion{0, apiV1, apiV2} {
				want := test.v1
				if version == apiV2 {
					want = test.v2
				}
				got := writeUnder(t, version, test.write)
				if got.status != want.status || got.code != want.code {
					t.Errorf("version %d: answered %d %s (%s), want %d %s", version, got.status, got.code, got.body, want.status, want.code)
				}
				if test.v1Exact != "" && version != apiV2 && got.body != test.v1Exact {
					t.Errorf("version %d: body %q, want the bytes /api/v1 always wrote, %q", version, got.body, test.v1Exact)
				}
			}
		})
	}
}
