package specgraph

// Red test D2 of the acceptance of t-format at cc00ab616a7849364d8ee9651947f85655f1d903. Copy it into
// packages/envoy/internal/dispatch/specgraph/ and run:
//
//	go test -count=1 -run TestPrintWritesNoPlainScalarThatAYAML11ReaderTypes ./internal/dispatch/specgraph/
//
// The printer writes a scalar plain when yaml.v3 reads the plain form back as the same string. A
// YAML 1.1 reader, such as PyYAML (the YAML library of Python), resolves more plain scalars to other
// types: "=" (value) and "<<" (merge) make PyYAML refuse the whole file, and "0x_" or
// "2026-11-16T10:00:00" read as an error or a datetime. The patterns are PyYAML 6.0.1's implicit
// resolvers (yaml/resolver.py), written without the verbose mode; the YAML 1.2 core patterns (the
// TypeScript yaml library) are here too, so the test holds the printer to both.

import (
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

var typedPlain = []struct {
	reader, tag string
	re          *regexp.Regexp
}{
	{"YAML 1.1", "null", regexp.MustCompile(`^(?:~|null|Null|NULL|)$`)},
	{"YAML 1.1", "bool", regexp.MustCompile(`^(?:yes|Yes|YES|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF)$`)},
	{"YAML 1.1", "int", regexp.MustCompile(`^(?:[-+]?0b[0-1_]+|[-+]?0[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$`)},
	{"YAML 1.1", "float", regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+][0-9]+)?|\.[0-9][0-9_]*(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)},
	{"YAML 1.1", "timestamp", regexp.MustCompile(`^(?:[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]|[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)[0-9][0-9]?:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)$`)},
	{"YAML 1.1", "merge", regexp.MustCompile(`^<<$`)},
	{"YAML 1.1", "value", regexp.MustCompile(`^=$`)},
	{"YAML 1.1", "yaml", regexp.MustCompile(`^(?:!|&|\*)$`)},
	{"YAML 1.2 core", "null", regexp.MustCompile(`^(?:~|null|Null|NULL|)$`)},
	{"YAML 1.2 core", "bool", regexp.MustCompile(`^(?:true|True|TRUE|false|False|FALSE)$`)},
	{"YAML 1.2 core", "int", regexp.MustCompile(`^(?:[-+]?[0-9]+|0o[0-7]+|0x[0-9a-fA-F]+)$`)},
	{"YAML 1.2 core", "float", regexp.MustCompile(`^(?:[-+]?(?:\.[0-9]+|[0-9]+(?:\.[0-9]*)?)(?:[eE][-+]?[0-9]+)?|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)},
}

func TestPrintWritesNoPlainScalarThatAYAML11ReaderTypes(t *testing.T) {
	t.Parallel()
	texts := append([]string{
		"=", "<<", "0x_", "2026-11-16T10:00:00", "2026-11-16 10:00:00 +01:00", "2026-11-16 10:00:00 Z",
		"2026-11-16T10:00:00 +01:00", "2026-1-5 10:00:00",
	}, hardTexts...)
	for _, text := range texts {
		document, err := printNode(Term{ID: "typed", Name: "typed term", Definition: text})
		if err != nil {
			t.Fatalf("printNode(%q): %v", text, err)
		}
		var root yaml.Node
		if err := yaml.Unmarshal([]byte(document), &root); err != nil {
			t.Fatalf("the document of %q does not read: %v", text, err)
		}
		var walk func(n *yaml.Node)
		walk = func(n *yaml.Node) {
			if n.Kind == yaml.ScalarNode && n.Style == 0 {
				for _, p := range typedPlain {
					if p.re.MatchString(n.Value) {
						t.Errorf("%q prints as the plain scalar %q, which a %s reader reads as a %s, not a string", text, n.Value, p.reader, p.tag)
					}
				}
			}
			for _, child := range n.Content {
				walk(child)
			}
		}
		walk(&root)
	}
}
