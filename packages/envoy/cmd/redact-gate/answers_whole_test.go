package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// wholeModule is a module of the forms an answer's words reach a printer through, each function
// one form. Package q defines a type over a response; package r reads one through it and imports
// no net/http of its own, so the census meets that type only as q's export data writes it.
var wholeModule = map[string]string{
	"go.mod": "module example.com/m\n\ngo 1.27\n",
	"q/q.go": `package q

import "net/http"

// Raw is a type another package defines over a response.
type Raw http.Response
`,
	"r/r.go": `package r

import (
	"io"

	"example.com/m/q"
)

func otherPackageBody(resp *q.Raw) io.Reader { return resp.Body }

func otherPackageStatus(resp *q.Raw) string { return resp.Status }
`,
	"p/p.go": `package p

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

func plainWhole(resp *http.Response) { fmt.Printf("%+v\n", resp) }

func plainBody(resp *http.Response) io.Reader { return resp.Body }

type rawResponse http.Response

func definedBody(resp *rawResponse) io.Reader { return resp.Body }

func definedStatus(resp *rawResponse) string { return resp.Status }

func definedWhole(resp *rawResponse) { fmt.Printf("%+v\n", resp) }

type responsePointer *http.Response

func namedPointerBody(resp responsePointer) io.Reader { return resp.Body }

func sliceWhole(resp *http.Response) { fmt.Printf("%+v\n", []http.Response{*resp}) }

type held struct{ resp http.Response }

func structWhole(resp *http.Response) { fmt.Printf("%+v\n", held{*resp}) }

func arrayWhole(resp *http.Response) { fmt.Printf("%+v\n", [1]http.Response{*resp}) }

func mapWhole(resp *http.Response) { fmt.Printf("%+v\n", map[string]*http.Response{"a": resp}) }

type pointerHeld struct{ Resp *http.Response }

func pointerFieldWhole(resp *http.Response) { fmt.Printf("%+v\n", &pointerHeld{resp}) }

type embeddedHeld struct{ pointerHeld }

func embeddedWhole(resp *http.Response) { fmt.Printf("%+v\n", embeddedHeld{pointerHeld{resp}}) }

type addressHeld struct {
	resp *http.Response
	Kept *http.Response ` + "`json:\"-\"`" + `
}

func addressOnly(resp *http.Response) { fmt.Printf("%+v\n", &addressHeld{resp, resp}) }

type node struct {
	next *node
	all  []node
	resp http.Response
}

func recursiveWhole(n *node) { fmt.Printf("%+v\n", n) }

type cleanNode struct {
	next *cleanNode
	all  map[string]cleanNode
	name string
}

func recursiveClean(n *cleanNode) { fmt.Printf("%+v\n", n) }

func conversionWhole(resp *http.Response) { fmt.Printf("%+v\n", any(resp)) }

func varWhole(resp *http.Response) {
	var v any = resp
	fmt.Printf("%+v\n", v)
}

func assignWhole(resp *http.Response) {
	var v any
	v = resp
	fmt.Printf("%+v\n", v)
}

func elementWhole(resp *http.Response) { fmt.Printf("%+v\n", []any{resp}) }

type anyHolder struct{ v any }

func fieldWhole(resp *http.Response) { fmt.Printf("%+v\n", anyHolder{v: resp}) }

func returnWhole(resp *http.Response) any { return resp }

func sendWhole(resp *http.Response, out chan<- any) { out <- resp }

func mapKeyWhole(resp *http.Response, seen map[any]bool) { seen[resp] = true }

func rangeWhole(resps []*http.Response) {
	var v any
	for _, v = range resps {
		fmt.Printf("%+v\n", v)
	}
}

func show[T any](v T) { fmt.Printf("%+v\n", v) }

func genericWhole(resp *http.Response) { show(resp) }

func statusCode(resp *http.Response) { fmt.Println(resp.StatusCode, resp.Header.Get("Date")) }

func cleanAny(code int) any { return code }

type respErr struct{ Resp *http.Response }

func (e *respErr) Error() string { return "refused" }

func errorsAsTarget(err error) bool {
	var target *respErr
	return errors.As(err, &target)
}

func errorsAsSource(e *respErr) bool {
	var target *respErr
	return errors.As(e, &target)
}

func respErrReturn(e *respErr) error { return e }

// ExternalError stands for an external error type (gate.external_errors) that holds a response.
type ExternalError struct{ Resp *http.Response }

func (e *ExternalError) Error() string { return "refused" }

func externalReturn(e *ExternalError) error { return e }
`,
}

// wholeCensus writes wholeModule to a scratch directory, lists packages p and r with their export
// data as the census lists a module's (goList), and runs the census over each (goPackage). It
// answers the answer census's findings.
func wholeCensus(t *testing.T) []finding {
	t.Helper()
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	dir := t.TempDir()
	for name, text := range wholeModule {
		path := filepath.Join(dir, "m", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	modDir := filepath.Join(dir, "m")
	listed, err := goList(modDir, hostBuild(), true, "./p", "./r")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range listed {
		if p.Error != nil {
			t.Fatalf("%s does not build: %s", p.ImportPath, p.Error.Err)
		}
	}
	build := newGoBuild(listed)
	g := &gate{}
	g.inv.Gate.Box = boxConfig{Type: "example.com/box.Text", Field: "text"}
	g.inv.Gate.External = []string{"example.com/m/p.ExternalError"}
	var c census
	for _, p := range listed {
		if p.ImportPath == "example.com/m/p" || p.ImportPath == "example.com/m/r" {
			g.goPackage(&c, dir, p.ImportPath, build, false, nil, nil, nil)
		}
	}
	if len(c.Unknown) > 0 {
		t.Fatalf("the census could not read the module: %s", strings.Join(c.Unknown, "; "))
	}
	return c.Answers
}

// TestTheAnswerCensusRefusesAResponseReachingAPrinterInEveryForm: the answer census refuses a
// response's Body or Status read through a type defined over net/http.Response (in its own package
// or another's, or a defined pointer type), and a value a printer reaches a response in - by value
// in a slice, an array, a struct or a recursive type, as fmt does, or through an exported field, a
// promoted one or a map's value behind pointers, as encoding/json does - made an interface value or
// a type parameter's value in every way Go makes one: a call's argument, a conversion, a
// variable's value, an assignment, a composite literal's element, a return, a send, a map key, a
// range's assignment, and a generic function's argument. Values that hold no response pass, and so
// does one that holds it only where fmt prints an address and encoding/json does not look (an
// unexported pointer, a field tagged json:"-"); a recursive type ends the walk. errors.As's target
// and an external error type's value are listed and pass, each with why.
func TestTheAnswerCensusRefusesAResponseReachingAPrinterInEveryForm(t *testing.T) {
	findings := wholeCensus(t)
	refused := map[string][]string{} // function -> the findings refused in it
	passed := map[string][]string{}  // function -> the whole-response findings listed and passed in it
	for _, f := range findings {
		name := f.Function[strings.LastIndex(f.Function, ".")+1:]
		switch {
		case !f.OK:
			refused[name] = append(refused[name], f.Kind+": "+f.What+": "+f.Why)
		case f.Kind == "whole-response":
			passed[name] = append(passed[name], f.What+": "+f.Why)
		}
	}
	// Control: the census read the package, and refuses the forms it refused before.
	for _, control := range []string{"plainWhole", "plainBody"} {
		if len(refused[control]) == 0 {
			t.Fatalf("control: the census refused nothing in %s, so it read nothing of the module; it refused: %v", control, refused)
		}
	}
	for _, c := range []struct{ function, kind, why string }{
		{"definedBody", "body", "a read of an answer's body"},
		{"definedStatus", "status", "a read of an answer's status line"},
		{"definedWhole", "whole-response", "a response handed whole"},
		{"namedPointerBody", "body", "a read of an answer's body"},
		{"otherPackageBody", "body", "a read of an answer's body"},
		{"otherPackageStatus", "status", "a read of an answer's status line"},
		{"sliceWhole", "whole-response", "a response handed whole"},
		{"structWhole", "whole-response", "a response handed whole"},
		{"arrayWhole", "whole-response", "a response handed whole"},
		{"mapWhole", "whole-response", "a response handed whole"},
		{"pointerFieldWhole", "whole-response", "a response handed whole"},
		{"embeddedWhole", "whole-response", "a response handed whole"},
		{"recursiveWhole", "whole-response", "a response handed whole"},
		{"conversionWhole", "whole-response", "a conversion"},
		{"varWhole", "whole-response", "a variable's value"},
		{"assignWhole", "whole-response", "an assignment"},
		{"elementWhole", "whole-response", "a composite literal's element"},
		{"fieldWhole", "whole-response", "a composite literal's element"},
		{"returnWhole", "whole-response", "a return"},
		{"sendWhole", "whole-response", "a send"},
		{"mapKeyWhole", "whole-response", "a map key"},
		{"rangeWhole", "whole-response", "a range's assignment"},
		{"genericWhole", "whole-response", "a type parameter"},
		{"errorsAsSource", "whole-response", "an argument of errors.As of type error"},
		{"respErrReturn", "whole-response", "a return"},
	} {
		t.Run(c.function, func(t *testing.T) {
			for _, got := range refused[c.function] {
				if strings.HasPrefix(got, c.kind+": ") && strings.Contains(got, c.why) {
					return
				}
			}
			t.Errorf("the census does not refuse %s as %s naming %q; it refused there: %q", c.function, c.kind, c.why, refused[c.function])
		})
	}
	for _, clean := range []string{"recursiveClean", "addressOnly", "statusCode", "cleanAny", "show"} {
		t.Run(clean, func(t *testing.T) {
			if len(refused[clean]) > 0 {
				t.Errorf("the census refuses %s, which hands out no response: %q", clean, refused[clean])
			}
		})
	}
	for _, c := range []struct{ function, why string }{
		{"errorsAsTarget", "errors.As's target"},
		{"errorsAsSource", "errors.As's target"},
		{"externalReturn", "an external error type's value"},
	} {
		t.Run(c.function+"/listed", func(t *testing.T) {
			if !slices.ContainsFunc(passed[c.function], func(p string) bool { return strings.Contains(p, c.why) }) {
				t.Errorf("the census does not list %s's value as passed for %q; it passed there: %q", c.function, c.why, passed[c.function])
			}
			if c.function != "errorsAsSource" && len(refused[c.function]) > 0 {
				t.Errorf("the census refuses %s: %q", c.function, refused[c.function])
			}
		})
	}
}
