package yamlerr

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Each of yaml.v3's error kinds that quotes the document, with a made-up credential where a
// person can paste one, becomes fixed words naming the line yaml.v3 names: a scalar where a
// mapping goes (a TypeError quotes the scalar's head), an unknown anchor (its error names the
// anchor, and no line), and a syntax error. The control is yaml.v3's own text, which holds the
// credential's head for the first two.
func TestReasonQuotesNothingOfTheDocument(t *testing.T) {
	const credential = "madeUpYamlCredential0123"
	for _, tc := range []struct {
		name, document, want string
		into                 any
	}{
		{"a scalar where a mapping goes", "github.com: " + credential + "\n",
			"the YAML holds a value of another kind than the file takes at line 1",
			&struct {
				GitHub struct{ Token string } `yaml:"github.com"`
			}{}},
		{"an unknown anchor", "token: *" + credential + "\n", "the YAML does not parse", &yaml.Node{}},
		{"a syntax error", "token: [" + credential + "\n", "the YAML does not parse at line 1", &yaml.Node{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := yaml.Unmarshal([]byte(tc.document), tc.into)
			if err == nil {
				t.Fatalf("yaml.Unmarshal(%q) = nil, want an error", tc.document)
			}
			if got := Reason(err); got != tc.want || strings.Contains(got, credential[:7]) {
				t.Fatalf("Reason = %q, want %q (yaml.v3's own text: %q)", got, tc.want, err)
			}
		})
	}
}
