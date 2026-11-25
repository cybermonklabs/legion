package main

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// The base and head of one shared test file, as a leak row's base and head hold it: the head
// changed a constant's value, a struct's fields and a method, and added a method and a constant.
const (
	spliceBase = `package p

import "strings"

// token is the read's token.
const token = "ghs_read"

const (
	// first is the first token.
	first = "ghs_first"
	cacheToken = "cache"
)

// fake answers a refusal.
type fake struct {
	refusing bool
}

func (f *fake) answer() string { return strings.Repeat("e", 3) }

// TestBase is a test of the shared file's that the head no longer holds.
func TestBase(t *testing.T) {}
`
	spliceHead = `package p

import "strings"

// token is the read's token, with the gate's marker.
const token = "ghs_read-not-a-secret-token"

// first is the first token, with the marker.
const first = "ghs_first-not-a-secret-token"

const (
	cacheToken = "cache"
	// added is a constant the base lacks.
	added = "x"
	implicitA = iota
	implicitB
	pairA, pairB = 1, 2
)

// fake answers a refusal, with words.
type fake struct {
	refusing    bool
	description string
}

func (f *fake) answer() string {
	if f.description != "" {
		return f.description
	}
	return strings.Repeat("e", 3)
}

// refuseWith refuses with description.
func (f *fake) refuseWith(description string) { f.refusing, f.description = true, description }

// TestHead needs a production API the base lacks.
func TestHead(t *testing.T) { _ = newAPI() }
`
)

// TestSpliceDeclarationsPutsTheHeadsTextOfEachNamedDeclarationAtTheBase: each named declaration's
// head text takes the place of the base's (a constant alone, a constant the base keeps in a
// group, a struct type, a method) or is added (a method and a constant the base lacks); the base's
// other declarations, its test among them, stay as the base wrote them, and the head's test that
// needs an API the base lacks does not come in; the result parses.
func TestSpliceDeclarationsPutsTheHeadsTextOfEachNamedDeclarationAtTheBase(t *testing.T) {
	names := []string{"p.token", "p.first", "p.fake", "(*p.fake).answer", "(*p.fake).refuseWith", "p.added"}
	out, err := spliceDeclarations([]byte(spliceHead), []byte(spliceBase), "p/p_test.go", "p", names)
	if err != nil {
		t.Fatalf("spliceDeclarations: %v", err)
	}
	got := string(out)
	if _, err := parser.ParseFile(token.NewFileSet(), "p_test.go", out, parser.ParseComments); err != nil {
		t.Fatalf("the spliced file does not parse: %v\n%s", err, got)
	}
	for _, want := range []string{
		`const token = "ghs_read-not-a-secret-token"`,
		"// first is the first token, with the marker.\n\tfirst = \"ghs_first-not-a-secret-token\"",
		"cacheToken = \"cache\"\n)",
		"description string",
		"return f.description",
		"func (f *fake) refuseWith(description string)",
		"// added is a constant the base lacks.\nconst added = \"x\"",
		"func TestBase(t *testing.T) {}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the spliced file holds no %q:\n%s", want, got)
		}
	}
	for _, absent := range []string{`"ghs_read"`, `"ghs_first"`, "TestHead", "newAPI"} {
		if strings.Contains(got, absent) {
			t.Errorf("the spliced file holds %q:\n%s", absent, got)
		}
	}
}

// TestSpliceDeclarationsRefusesANameItCannotTakeAlone: a name the head does not declare, one whose
// head text is the base's (it would put nothing at the base), a constant whose value the spec
// before it gives, and a name in a spec with another name are refused, each naming why.
func TestSpliceDeclarationsRefusesANameItCannotTakeAlone(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"p.missing", "declares no p.missing"},
		{"p.cacheToken", "is the base's, so naming it puts nothing at the base"},
		{"p.implicitB", "whose value the spec before it in its group gives"},
		{"p.pairA", "which a declaration entry cannot take apart"},
	} {
		_, err := spliceDeclarations([]byte(spliceHead), []byte(spliceBase), "p/p_test.go", "p", []string{tc.name})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error naming %q", tc.name, err, tc.want)
		}
	}
}
